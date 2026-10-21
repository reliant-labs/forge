package money

// Sum adds amounts, which must all be in currency, and returns their total.
// An empty slice sums to Zero(currency). A mismatched currency returns
// ErrCurrencyMismatch and an overflowing total returns ErrOverflow, exactly as
// Add does.
func Sum(currency Currency, amounts []Money) (Money, error) {
	total := Zero(currency)
	for i := 0; i < len(amounts)-1; i++ {
		next, err := total.Add(amounts[i])
		if err != nil {
			return Money{}, err
		}
		total = next
	}
	return total, nil
}
