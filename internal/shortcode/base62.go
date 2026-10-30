package shortcode

import (
	"math"
	"strings"
)

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func Encode(id int64) string {
	// so the scenario is we have a number of int64 and we have to convert that to base62

	if id == 0 {
		return string(alphabet[0])
	}
	var res []byte

	for id > 0 {
		digit := id % 62 // get the remainder
		res = append(res, alphabet[digit])
		id = id / 62
	}

	// Digits were produced right-to-left, so reverse them in place by
	// swapping from both ends toward the middle.

	j := len(res) - 1
	i := 0
	for i < j {
		res[i], res[j] = res[j], res[i]
		i++
		j--
	}

	return string(res)
}

// Decode is the inverse of Encode. ok is false for an empty string, a
// character outside the alphabet, or a value that doesn't fit in an int64.
func Decode(code string) (id int64, ok bool) {
	if code == "" || len(code) > 11 { // 62^11 is already past MaxInt64
		return 0, false
	}
	var n uint64
	for i := 0; i < len(code); i++ {
		d := strings.IndexByte(alphabet, code[i])
		if d < 0 {
			return 0, false
		}
		if n > (math.MaxInt64-uint64(d))/62 { // n*62+d would overflow
			return 0, false
		}
		n = n*62 + uint64(d)
	}
	return int64(n), true
}
