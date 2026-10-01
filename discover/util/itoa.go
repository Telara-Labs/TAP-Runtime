package util

func Itoa(x int) string {
	if x == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for x > 0 {
		i--
		d[i] = byte('0' + x%10)
		x /= 10
	}
	return string(d[i:])
}
