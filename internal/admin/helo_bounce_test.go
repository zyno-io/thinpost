package admin

import "testing"

func TestHeloUnderBounceDomain(t *testing.T) {
	for _, c := range [][2]string{
		{"smtp.hosting.example", "smtp.hosting.example"},
		{"smtp.mx.example.net", "mx.example.net"},
		{"smtp.mx.example.net", "example.net"},
	} {
		if !HeloUnderBounceDomain(c[0], c[1]) {
			t.Errorf("%s under %s refused", c[0], c[1])
		}
	}
	for _, c := range [][2]string{
		{"smtp.example.org", "other.example.org"},
		{"smtp.example.org", "mtp.example.org"},
		{"smtp.hosting.example", "hosting.test"},
		{"example.net", "smtp.example.net"},
	} {
		if HeloUnderBounceDomain(c[0], c[1]) {
			t.Errorf("%s under %s accepted", c[0], c[1])
		}
	}
}
