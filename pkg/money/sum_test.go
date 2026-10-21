package money

import "testing"

func TestSum(t *testing.T) {
	cases := []struct {
		name    string
		amounts []Money
		want    Money
	}{
		{"empty", nil, Zero(USD)},
		{"trailing zero", []Money{FromCents(150, USD), FromCents(250, USD), FromCents(0, USD)}, FromCents(400, USD)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sum(USD, tc.amounts)
			if err != nil {
				t.Fatalf("Sum: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("Sum = %s, want %s", got, tc.want)
			}
		})
	}
}
