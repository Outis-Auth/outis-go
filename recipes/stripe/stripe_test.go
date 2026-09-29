package stripe

import (
	"encoding/json"
	"maps"
	"os"
	"strings"
	"testing"
)

// TestRecipeVectors decodes each shared vector's Stripe params into its
// recipe type and pins what approvers see to what every Outis SDK shows.
func TestRecipeVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/recipe-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		DescriptionMax int `json:"description_max"`
		Cases          []struct {
			Name     string            `json:"name"`
			Action   string            `json:"action"`
			Params   json.RawMessage   `json:"params"`
			Customer string            `json:"customer"`
			Options  json.RawMessage   `json:"options"`
			Shown    map[string]string `json:"shown"`
			Error    string            `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.DescriptionMax != DescriptionLimit {
		t.Fatalf("description_max %d, DescriptionLimit %d", v.DescriptionMax, DescriptionLimit)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no recipe vectors")
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var shown map[string]string
			var err error
			switch c.Action {
			case ActionTransfersCreate:
				shown, err = decode[Transfer](t, c.Params, c.Options).ShowApprovers()
			case ActionPayoutsCreate:
				shown, err = decode[Payout](t, c.Params, c.Options).ShowApprovers()
			case ActionRefundsCreate:
				shown, err = decode[Refund](t, c.Params, c.Options).ShowApprovers()
			case ActionCustomersDelete:
				d := decode[CustomerDelete](t, c.Options)
				d.Customer = c.Customer
				shown, err = d.ShowApprovers()
			default:
				t.Fatalf("unknown action %s", c.Action)
			}
			if c.Error != "" {
				want := "stripe " + strings.TrimPrefix(c.Action, "stripe.") + " needs " + c.Error
				if err == nil || err.Error() != want || shown != nil {
					t.Fatalf("got %v, %v; want error %q", shown, err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(shown, c.Shown) {
				t.Errorf("\n got %v\nwant %v", shown, c.Shown)
			}
		})
	}
}

// decode unmarshals each JSON object into one T, so request options land
// beside the params the way stripe-go embeds them.
func decode[T any](t *testing.T, raws ...json.RawMessage) T {
	t.Helper()
	var v T
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
	}
	return v
}
