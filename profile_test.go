package main

import "testing"

func TestProfileAddress(t *testing.T) {
	cases := map[string]string{
		"server.example.com":      "server.example.com:22",
		"server.example.com:2222": "server.example.com:2222",
		"2001:db8::1":             "[2001:db8::1]:22",
		"[2001:db8::1]":           "[2001:db8::1]:22",
		"[2001:db8::1]:2222":      "[2001:db8::1]:2222",
	}
	for input, expected := range cases {
		if actual := profileAddress(input); actual != expected {
			t.Errorf("profileAddress(%q) = %q; want %q", input, actual, expected)
		}
	}
}
