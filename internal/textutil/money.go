package textutil

import "strconv"

// EUR is an amount of money as every text shows it, before its unit: with
// two decimals, "2.68", and an amount above zero that two decimals would
// show as nothing, "less than 0.01".
func EUR(v float64) string {
	if v > 0 && v < 0.005 {
		return "less than 0.01"
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// Rate is a price per unit, a line or an hour, before its unit: to four
// decimals, as a cent's fraction per line would read 0.00 at two.
func Rate(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }
