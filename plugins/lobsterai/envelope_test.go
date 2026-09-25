package main

import (
	"strings"
	"testing"
)

func TestParseEnvelope(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantOK      bool
		wantCode    int
		wantMessage string
		wantData    map[string]any
	}{
		{
			name:     "success with msg",
			body:     `{"code":0,"msg":"OK","data":{"accessToken":"a"}}`,
			wantOK:   true,
			wantData: map[string]any{"accessToken": "a"},
		},
		{
			name:     "success with message key",
			body:     `{"code":0,"message":"success","data":{"accessToken":"a"}}`,
			wantOK:   true,
			wantData: map[string]any{"accessToken": "a"},
		},
		{
			name:        "business failure keeps the server message",
			body:        `{"code":40101,"msg":"token rejected","data":null}`,
			wantCode:    40101,
			wantMessage: "token rejected",
		},
		{
			name:        "code as numeric string",
			body:        `{"code":"500","msg":"boom","data":{}}`,
			wantCode:    500,
			wantMessage: "boom",
		},
		{
			name:        "code 0 with null data is a failure and keeps the server message",
			body:        `{"code":0,"msg":"OK","data":null}`,
			wantMessage: "OK",
		},
		{
			name:        "code 0 with array data is a failure",
			body:        `{"code":0,"msg":"OK","data":[1,2]}`,
			wantMessage: "OK",
		},
		{
			name:        "code 0 with null data and no message",
			body:        `{"code":0,"data":null}`,
			wantMessage: "data 为空（accessToken 可能已失效）",
		},
		{
			name:        "missing code",
			body:        `{"msg":"weird","data":{}}`,
			wantCode:    -1,
			wantMessage: "weird",
		},
		{
			name:        "non-object body",
			body:        `[1,2,3]`,
			wantCode:    -1,
			wantMessage: "响应不是 JSON 对象",
		},
		{
			name:        "invalid json",
			body:        `<html>error</html>`,
			wantCode:    -1,
			wantMessage: "响应不是合法 JSON",
		},
		{
			name:        "empty body",
			body:        ``,
			wantCode:    -1,
			wantMessage: "响应体为空",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseEnvelope([]byte(test.body))
			if got.OK != test.wantOK {
				t.Fatalf("OK = %v, want %v (result %+v)", got.OK, test.wantOK, got)
			}
			if !test.wantOK && got.Code != test.wantCode {
				t.Fatalf("Code = %d, want %d", got.Code, test.wantCode)
			}
			if test.wantMessage != "" && got.Message != test.wantMessage {
				t.Fatalf("Message = %q, want %q", got.Message, test.wantMessage)
			}
			if test.wantData != nil {
				if got.Data == nil {
					t.Fatalf("Data = nil, want %v", test.wantData)
				}
				for key, want := range test.wantData {
					if got.Data[key] != want {
						t.Fatalf("Data[%s] = %v, want %v", key, got.Data[key], want)
					}
				}
			}
		})
	}
}

func TestEnvelopeFieldReaders(t *testing.T) {
	source := map[string]any{
		"text":     "value",
		"number":   float64(12),
		"numstr":   " 3.5 ",
		"badnum":   "abc",
		"boolTrue": true,
		"items":    []any{"a", 1, "b"},
		"nested":   map[string]any{"k": "v"},
	}
	if got := readStringField(source, "text"); got != "value" {
		t.Fatalf("readStringField(text) = %q", got)
	}
	if got := readStringField(source, "number"); got != "12" {
		t.Fatalf("readStringField(number) = %q, want 12", got)
	}
	if got := readStringField(source, "missing"); got != "" {
		t.Fatalf("readStringField(missing) = %q, want empty", got)
	}
	if got, ok := readNumberField(source, "numstr"); !ok || got != 3.5 {
		t.Fatalf("readNumberField(numstr) = %v,%v want 3.5,true", got, ok)
	}
	if _, ok := readNumberField(source, "badnum"); ok {
		t.Fatal("readNumberField(badnum) reported a number")
	}
	if _, ok := readNumberField(source, "text"); ok {
		t.Fatal("readNumberField(text) reported a number")
	}
	if !readBoolField(source, "boolTrue") || readBoolField(source, "text") {
		t.Fatal("readBoolField misread a value")
	}
	if got := readStringArray(source, "items"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("readStringArray = %v", got)
	}
	if got := asRecord(source["nested"]); got == nil || got["k"] != "v" {
		t.Fatalf("asRecord(nested) = %v", got)
	}
	if asRecord(source["items"]) != nil {
		t.Fatal("asRecord(array) must be nil")
	}
}

func TestIsNumericLiteral(t *testing.T) {
	tests := map[string]bool{
		"12": true, "-3": true, "3.5": true, "-0.25": true,
		"": false, "abc": false, "1.2.3": false, "1.": false, ".5": false,
		"1e5": false, "+1": false, "1 ": false,
	}
	for input, want := range tests {
		if got := isNumericLiteral(input); got != want {
			t.Fatalf("isNumericLiteral(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestStringifyValue(t *testing.T) {
	if got := stringifyValue("x"); got != "x" {
		t.Fatalf("stringifyValue(string) = %q", got)
	}
	if got := stringifyValue(float64(2)); got != "2" {
		t.Fatalf("stringifyValue(number) = %q", got)
	}
	if got := stringifyValue(nil); got != "" {
		t.Fatalf("stringifyValue(nil) = %q", got)
	}
	if got := stringifyValue(map[string]any{"a": "b"}); !strings.Contains(got, `"a":"b"`) {
		t.Fatalf("stringifyValue(object) = %q", got)
	}
}
