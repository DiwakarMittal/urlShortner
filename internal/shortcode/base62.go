package shortcode

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
