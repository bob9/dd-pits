package gemini

import (
	"fmt"
	"testing"
)

func TestParseGeminiResponseReceiptCurrency(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		want     string
		wantNil  bool
	}{
		{name: "ambiguous dollar remains unknown", currency: "null", wantNil: true},
		{name: "explicit currency is normalised", currency: `"usd"`, want: "USD"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			extractedJSON := fmt.Sprintf(`{"vendor":"Fatstrapfpv","abn":null,"invoice":"072026","date":"2026-07-20","currency":%s,"line_items":[],"subtotal":305.10,"gst":0,"total":305.10}`, tc.currency)
			body := []byte(fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":%q}]}}]}`, extractedJSON))
			got, err := parseGeminiResponse(body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.wantNil {
				if got.Currency != nil {
					t.Fatalf("currency = %q, want nil", *got.Currency)
				}
				return
			}
			if got.Currency == nil || *got.Currency != tc.want {
				t.Fatalf("currency = %v, want %q", got.Currency, tc.want)
			}
		})
	}
}
