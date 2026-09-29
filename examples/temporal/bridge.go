//go:build temporal

package main

import (
	"io"
	"net/http"

	outis "github.com/outis-auth/outis-go"
	"go.temporal.io/sdk/client"
)

// Bridge turns a verified Outis webhook into the workflow's decision signal.
// Outis retries a delivery that doesn't get a 2xx.
func Bridge(tc client.Client, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		ev, err := outis.VerifyWebhook(body, r.Header, secret, nil)
		if err != nil {
			http.Error(w, "bad signature", http.StatusBadRequest)
			return
		}
		req := ev.Data.Request
		wf := req.Params["workflow_id"]
		if wf == "" || req.Outcome == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if err := tc.SignalWorkflow(r.Context(), wf, "", DecisionSignal, string(req.Outcome)); err != nil {
			http.Error(w, "signal failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
