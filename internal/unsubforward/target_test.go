package unsubforward

import (
	"strings"
	"testing"
)

func one(value, post string) Original {
	o := Original{ListFields: 1, Value: value}
	if post != "" {
		o.PostFields, o.PostValue = 1, post
	}
	return o
}

func TestDecide(t *testing.T) {
	long := "<https://example.com/" + strings.Repeat("a", 2100) + ">"
	for name, tc := range map[string]struct {
		in     Original
		url    string
		host   string
		reason string
	}{
		"https with one-click post": {one("<https://unsub.example.com/u?t=abc>", OneClickPost), "https://unsub.example.com/u?t=abc", "unsub.example.com", ""},
		"host is lowercased":        {one("<https://Unsub.Example.COM/U>", OneClickPost), "https://unsub.example.com/U", "unsub.example.com", ""},
		"explicit port 443":         {one("<https://unsub.example.com:443/u>", OneClickPost), "https://unsub.example.com/u", "unsub.example.com", ""},
		"scheme in capitals":        {one("<HTTPS://unsub.example.com/u>", OneClickPost), "https://unsub.example.com/u", "unsub.example.com", ""},
		"mailto first, https next":  {one("<mailto:u@example.com>, <https://unsub.example.com/u>", OneClickPost), "https://unsub.example.com/u", "unsub.example.com", ""},
		"first https of several":    {one("<https://first.example.com/u>, <https://second.example.com/u>", OneClickPost), "https://first.example.com/u", "first.example.com", ""},
		"comments and white space":  {one("  (opt out) <https://unsub.example.com/u> ( or (nested) mail ) , <mailto:u@example.com>\t", OneClickPost), "https://unsub.example.com/u", "unsub.example.com", ""},
		"post value with spaces":    {one("<https://unsub.example.com/u>", " List-Unsubscribe=One-Click "), "https://unsub.example.com/u", "unsub.example.com", ""},

		"https without post":      {one("<https://unsub.example.com/u>", ""), "", "", ReasonNoOneClick},
		"https with other post":   {one("<https://unsub.example.com/u>", "List-Unsubscribe=one-click"), "", "", ReasonNoOneClick},
		"post for https is wrong": {one("<https://unsub.example.com/u>", "something else"), "", "", ReasonNoOneClick},
		"mailto only":             {one("<mailto:u@example.com>", OneClickPost), "", "", ReasonMailtoOnly},
		"mailto only, no post":    {one("<mailto:u@example.com?subject=x>", ""), "", "", ReasonMailtoOnly},
		"only an http link":       {one("<http://unsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},
		"only another scheme":     {one("<ftp://unsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},

		"junk":                                  {one("https://unsub.example.com/u", OneClickPost), "", "", ReasonUnusable},
		"empty value":                           {one("", OneClickPost), "", "", ReasonUnusable},
		"empty brackets":                        {one("<>", OneClickPost), "", "", ReasonUnusable},
		"text after":                            {one("<https://unsub.example.com/u> junk", OneClickPost), "", "", ReasonUnusable},
		"trailing comma":                        {one("<https://unsub.example.com/u>,", OneClickPost), "", "", ReasonUnusable},
		"missing bracket":                       {one("<https://unsub.example.com/u", OneClickPost), "", "", ReasonUnusable},
		"space inside":                          {one("<https://unsub.example.com/a b>", OneClickPost), "", "", ReasonUnusable},
		"unbalanced comment":                    {one("(oops <https://unsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},
		"comment too deep":                      {one("((((((x)))))) <https://unsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},
		"over-long value":                       {one(long, OneClickPost), "", "", ReasonUnusable},
		"too many items":                        {one(strings.Repeat("<mailto:a@example.com>,", 9)+"<https://unsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},
		"non-ASCII value":                       {one("<https://unsub.example.com/ü>", OneClickPost), "", "", ReasonUnusable},
		"non-ASCII host":                        {one("<https://ünsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},
		"control character":                     {one("<https://unsub.example.com/\x01>", OneClickPost), "", "", ReasonUnusable},
		"userinfo":                              {one("<https://user:pw@unsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},
		"IPv4 literal":                          {one("<https://203.0.113.9/u>", OneClickPost), "", "", ReasonUnusable},
		"private IPv4 literal":                  {one("<https://10.0.0.1/u>", OneClickPost), "", "", ReasonUnusable},
		"loopback":                              {one("<https://127.0.0.1/u>", OneClickPost), "", "", ReasonUnusable},
		"IPv6 literal":                          {one("<https://[::1]/u>", OneClickPost), "", "", ReasonUnusable},
		"localhost":                             {one("<https://localhost/u>", OneClickPost), "", "", ReasonUnusable},
		"cluster name":                          {one("<https://"+"api.default"+".svc/u>", OneClickPost), "", "", ReasonUnusable},
		"single label":                          {one("<https://"+"intranet"+"/u>", OneClickPost), "", "", ReasonUnusable},
		"other port":                            {one("<https://unsub.example.com:8443/u>", OneClickPost), "", "", ReasonUnusable},
		"fragment":                              {one("<https://unsub.example.com/u#x>", OneClickPost), "", "", ReasonUnusable},
		"first https is bad, later one is fine": {one("<https://10.0.0.1/u>, <https://unsub.example.com/u>", OneClickPost), "", "", ReasonUnusable},
		"repeated list fields":                  {Original{ListFields: 2, PostFields: 1, PostValue: OneClickPost}, "", "", ReasonUnusable},
		"repeated post fields":                  {Original{ListFields: 1, Value: "<https://unsub.example.com/u>", PostFields: 2}, "", "", ReasonUnusable},
		"post without list":                     {Original{PostFields: 1, PostValue: OneClickPost}, "", "", ReasonUnusable},
	} {
		t.Run(name, func(t *testing.T) {
			target, reason := Decide(tc.in)
			if reason != tc.reason || target.URL != tc.url || target.Host != tc.host {
				t.Fatalf("got %+v %q, want %q %q %q", target, reason, tc.url, tc.host, tc.reason)
			}
		})
	}
}

func TestPresent(t *testing.T) {
	if (Original{}).Present() || !(Original{ListFields: 1}).Present() || !(Original{PostFields: 1}).Present() {
		t.Fatal("Present")
	}
}
