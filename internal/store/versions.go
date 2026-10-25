package store

import "unicode"

// CompareVersions orders version strings with their digit runs compared as numbers ("v1.9.0" before
// "v1.10.0"), then bytewise.
func CompareVersions(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			ei, ej := i, j
			for ei < len(a) && isDigit(a[ei]) {
				ei++
			}
			for ej < len(b) && isDigit(b[ej]) {
				ej++
			}
			if c := compareNumbers(a[i:ei], b[j:ej]); c != 0 {
				return c
			}
			i, j = ei, ej
			continue
		}
		if a[i] != b[j] {
			if a[i] < b[j] {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case len(a)-i < len(b)-j:
		return -1
	case len(a)-i > len(b)-j:
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func isDigit(c byte) bool { return c < 0x80 && unicode.IsDigit(rune(c)) }

func compareNumbers(a, b string) int {
	for len(a) > 1 && a[0] == '0' {
		a = a[1:]
	}
	for len(b) > 1 && b[0] == '0' {
		b = b[1:]
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
