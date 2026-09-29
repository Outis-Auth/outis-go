// Package stripe builds what approvers see for common stripe-go calls,
// without importing stripe-go. Each type copies the matching params
// struct's fields (same names, same pointer types), checked against
// https://docs.stripe.com/api/transfers/create,
// https://docs.stripe.com/api/payouts/create,
// https://docs.stripe.com/api/refunds/create and
// https://docs.stripe.com/api/customers/delete.
//
// ShowApprovers returns only ids, amounts in minor units, currencies, a few
// enum values and flags, a capped description and the connected account.
// A nil or empty field is left out, and a call missing a field Stripe
// requires returns an error instead, so approvers never see half an
// operation. Every Outis SDK shows the same fields for the same call,
// pinned by testdata/recipe-vectors.json.
package stripe

import (
	"fmt"
	"strconv"
	"unicode/utf8"
)

// The action each recipe is for, for GuardOptions.Action.
const (
	ActionTransfersCreate = "stripe.transfers.create"
	ActionPayoutsCreate   = "stripe.payouts.create"
	ActionRefundsCreate   = "stripe.refunds.create"
	ActionCustomersDelete = "stripe.customers.delete"
)

// DescriptionLimit is the most characters of a description approvers see.
const DescriptionLimit = 64

// Transfer is the approval-relevant part of stripe-go's TransferCreateParams.
// StripeAccount is the connected account from the embedded Params.
type Transfer struct {
	Amount            *int64  `json:"amount"`
	Currency          *string `json:"currency"`
	Destination       *string `json:"destination"`
	SourceTransaction *string `json:"source_transaction"`
	Description       *string `json:"description"`
	StripeAccount     *string `json:"stripe_account"`
}

// ShowApprovers returns the transfer's amount, currency, destination
// account, source charge, capped description and connected account. It
// needs Amount, Currency and Destination.
func (t Transfer) ShowApprovers() (map[string]string, error) {
	if err := need("transfers.create", field{"amount", t.Amount == nil}, field{"currency", empty(t.Currency)}, field{"destination", empty(t.Destination)}); err != nil {
		return nil, err
	}
	m := map[string]string{}
	putInt(m, "amount", t.Amount)
	put(m, "currency", t.Currency)
	put(m, "destination", t.Destination)
	put(m, "source_transaction", t.SourceTransaction)
	putDescription(m, t.Description)
	put(m, "stripe_account", t.StripeAccount)
	return m, nil
}

// Payout is the approval-relevant part of stripe-go's PayoutCreateParams.
type Payout struct {
	Amount        *int64  `json:"amount"`
	Currency      *string `json:"currency"`
	Destination   *string `json:"destination"`
	Method        *string `json:"method"`
	SourceType    *string `json:"source_type"`
	Description   *string `json:"description"`
	StripeAccount *string `json:"stripe_account"`
}

// ShowApprovers returns the payout's amount, currency, destination, method,
// source type, capped description and connected account, each only when set.
// It needs Amount and Currency.
func (p Payout) ShowApprovers() (map[string]string, error) {
	if err := need("payouts.create", field{"amount", p.Amount == nil}, field{"currency", empty(p.Currency)}); err != nil {
		return nil, err
	}
	m := map[string]string{}
	putInt(m, "amount", p.Amount)
	put(m, "currency", p.Currency)
	put(m, "destination", p.Destination)
	put(m, "method", p.Method)
	put(m, "source_type", p.SourceType)
	putDescription(m, p.Description)
	put(m, "stripe_account", p.StripeAccount)
	return m, nil
}

// Refund is the approval-relevant part of stripe-go's RefundCreateParams.
type Refund struct {
	Charge               *string `json:"charge"`
	PaymentIntent        *string `json:"payment_intent"`
	Amount               *int64  `json:"amount"`
	Reason               *string `json:"reason"`
	ReverseTransfer      *bool   `json:"reverse_transfer"`
	RefundApplicationFee *bool   `json:"refund_application_fee"`
	StripeAccount        *string `json:"stripe_account"`
}

// ShowApprovers returns what's being refunded, how much and how. With no
// Amount, Stripe refunds everything left, so amount reads "full". It needs
// Charge or PaymentIntent.
func (r Refund) ShowApprovers() (map[string]string, error) {
	if err := need("refunds.create", field{"charge or payment_intent", empty(r.Charge) && empty(r.PaymentIntent)}); err != nil {
		return nil, err
	}
	m := map[string]string{"amount": "full"}
	put(m, "charge", r.Charge)
	put(m, "payment_intent", r.PaymentIntent)
	putInt(m, "amount", r.Amount)
	put(m, "reason", r.Reason)
	putBool(m, "reverse_transfer", r.ReverseTransfer)
	putBool(m, "refund_application_fee", r.RefundApplicationFee)
	put(m, "stripe_account", r.StripeAccount)
	return m, nil
}

// CustomerDelete is the id passed to stripe-go's V1Customers.Delete and the
// connected account from its CustomerDeleteParams.
type CustomerDelete struct {
	Customer      string  `json:"customer"`
	StripeAccount *string `json:"stripe_account"`
}

// ShowApprovers returns the customer id and connected account. Deleting a
// customer can't be undone. It needs Customer.
func (c CustomerDelete) ShowApprovers() (map[string]string, error) {
	if err := need("customers.delete", field{"customer", c.Customer == ""}); err != nil {
		return nil, err
	}
	m := map[string]string{}
	put(m, "customer", &c.Customer)
	put(m, "stripe_account", c.StripeAccount)
	return m, nil
}

type field struct {
	name    string
	missing bool
}

// need names the first missing required field, in the same words every
// Outis SDK uses.
func need(method string, fields ...field) error {
	for _, f := range fields {
		if f.missing {
			return fmt.Errorf("stripe %s needs %s", method, f.name)
		}
	}
	return nil
}

func empty(v *string) bool {
	return v == nil || *v == ""
}

func put(m map[string]string, k string, v *string) {
	if v != nil && *v != "" {
		m[k] = *v
	}
}

func putInt(m map[string]string, k string, v *int64) {
	if v != nil {
		m[k] = strconv.FormatInt(*v, 10)
	}
}

func putBool(m map[string]string, k string, v *bool) {
	if v != nil {
		m[k] = strconv.FormatBool(*v)
	}
}

// putDescription cuts to DescriptionLimit characters, ending in "..." when cut.
func putDescription(m map[string]string, v *string) {
	if v == nil || *v == "" {
		return
	}
	s := *v
	if utf8.RuneCountInString(s) > DescriptionLimit {
		s = string([]rune(s)[:DescriptionLimit-3]) + "..."
	}
	m["description"] = s
}
