package validator

import "testing"

func TestRejectPrivateAndWide(t *testing.T) {
	got, err := Prefixes([]string{"10.0.0.0/8", "8.0.0.0/8", "8.8.8.0/24"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "8.8.8.0/24" {
		t.Fatalf("got %v", got)
	}
}
