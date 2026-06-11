package mathutil

// Sum returns the arithmetic sum of values.
func Sum(values []int) int {
	total := 0
	for _, v := range values {
		total += v
	}
	return total
}
