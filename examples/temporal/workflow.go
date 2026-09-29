//go:build temporal

package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	outis "github.com/outis-auth/outis-go"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// DecisionSignal is the signal the bridge sends with the request's outcome.
const DecisionSignal = "outisDecision"

// PayoutInput is what the workflow is asked to pay.
type PayoutInput struct {
	Requester string
	Account   string
	Cents     int64
}

// PayoutWorkflow proposes a sealed payout, waits up to three days for the
// operators, and has a worker run it once authorized.
func PayoutWorkflow(ctx workflow.Context, in PayoutInput) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 2 * time.Minute})
	var a *Activities

	var requestID string
	if err := workflow.ExecuteActivity(ctx, a.Propose, in).Get(ctx, &requestID); err != nil {
		return "", err
	}

	var outcome string
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(workflow.GetSignalChannel(ctx, DecisionSignal), func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, &outcome)
	})
	sel.AddFuture(workflow.NewTimer(ctx, 72*time.Hour), func(workflow.Future) { outcome = "timeout" })
	sel.Select(ctx)
	if outcome != string(outis.OutcomeAuthorized) {
		return "", fmt.Errorf("payout %s not authorized: %s", requestID, outcome)
	}

	var ref string
	err := workflow.ExecuteActivity(ctx, a.Execute, requestID).Get(ctx, &ref)
	return ref, err
}

// Activities hold the Outis client that proposes and the worker that runs.
type Activities struct {
	Outis  *outis.Client
	Worker *outis.Worker
}

// Propose seals the payout and creates the request. The idempotency key
// comes from the workflow id, so a retried activity gets the same request.
func (a *Activities) Propose(ctx context.Context, in PayoutInput) (string, error) {
	wf := activity.GetInfo(ctx).WorkflowExecution.ID
	d, err := a.Outis.Defer(ctx, outis.GuardOptions{
		Action:    "payouts.release",
		Requester: in.Requester,
		ShowApprovers: map[string]string{
			"to":          in.Account,
			"cents":       strconv.FormatInt(in.Cents, 10),
			"workflow_id": wf,
		},
		IdempotencyKey: "temporal:" + wf,
		DeferTo:        &outis.DeferTo{Worker: "payouts", Call: "release", Args: []any{in.Account, in.Cents}},
	})
	if err != nil {
		return "", err
	}
	return d.ID, nil
}

// Execute claims, verifies and runs the intent. A retry after the run was
// reported reads the reference back instead of running again.
func (a *Activities) Execute(ctx context.Context, requestID string) (string, error) {
	res, err := a.Worker.Execute(ctx, requestID)
	var apiErr *outis.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "already_reported" {
		r, err := a.Outis.Requests.Retrieve(ctx, requestID)
		if err != nil {
			return "", err
		}
		if r.Execution.State == outis.ExecutionSucceeded {
			return r.Execution.Reference, nil
		}
		return "", temporal.NewNonRetryableApplicationError(r.Execution.Error, "outis_execution_failed", nil)
	}
	if err != nil {
		return "", err
	}
	if res.Status != outis.ExecutionSucceeded {
		return "", temporal.NewNonRetryableApplicationError(res.Err.Error(), "outis_execution_failed", res.Err)
	}
	return res.Reference, nil
}
