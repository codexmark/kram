package provider

import (
	"strings"
	"testing"
)

func TestScanSSEDataCallsFnOncePerDataLine(t *testing.T) {
	input := "event: message\ndata: one\n\ndata: two\n\n: a comment\nid: 5\ndata: three\n\n"
	var got []string
	if err := scanSSEData(strings.NewReader(input), func(data string) bool {
		got = append(got, data)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestScanSSEDataSkipsBlankDataPayloads(t *testing.T) {
	input := "data: \ndata:\ndata: real\n\n"
	var got []string
	if err := scanSSEData(strings.NewReader(input), func(data string) bool {
		got = append(got, data)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "real" {
		t.Errorf("got %v, want just [\"real\"]", got)
	}
}

func TestScanSSEDataStopsWhenFnReturnsFalse(t *testing.T) {
	input := "data: one\n\ndata: two\n\ndata: three\n\n"
	var got []string
	if err := scanSSEData(strings.NewReader(input), func(data string) bool {
		got = append(got, data)
		return data != "two"
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v, want reading to stop right after \"two\"", got)
	}
}

func TestScanSSEDataTrimsPrefixWhitespace(t *testing.T) {
	input := "data:   spaced out  \n\n"
	var got string
	if err := scanSSEData(strings.NewReader(input), func(data string) bool {
		got = data
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if got != "spaced out" {
		t.Errorf("got %q, want %q", got, "spaced out")
	}
}
