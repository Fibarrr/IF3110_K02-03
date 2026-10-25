package databasemanagement

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeEditableValue(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    any
		wantErr bool
	}{
		{name: "string", raw: `"hello"`, want: "hello"},
		{name: "number", raw: `42`, want: json.Number("42")},
		{name: "boolean", raw: `true`, want: true},
		{name: "null", raw: `null`, want: nil},
		{name: "object rejected", raw: `{"a":1}`, wantErr: true},
		{name: "array rejected", raw: `[1,2]`, wantErr: true},
		{name: "invalid JSON", raw: `x`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeEditableValue(json.RawMessage(test.raw))
			if test.wantErr {
				var inputErr InputError
				if !errors.As(err, &inputErr) {
					t.Fatalf("err = %v, mau InputError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("value = %#v (%T), mau %#v (%T)", got, got, test.want, test.want)
			}
		})
	}
}

func TestValidateRowKey(t *testing.T) {
	primaryKeys := []string{"tenant_id", "id"}
	if err := validateRowKey(map[string]json.RawMessage{"tenant_id": json.RawMessage(`1`), "id": json.RawMessage(`2`)}, primaryKeys); err != nil {
		t.Fatalf("primary key komposit valid ditolak: %v", err)
	}
	for name, key := range map[string]map[string]json.RawMessage{
		"missing key": {"id": json.RawMessage(`2`)},
		"extra key":   {"tenant_id": json.RawMessage(`1`), "id": json.RawMessage(`2`), "other": json.RawMessage(`3`)},
		"nil key":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			var inputErr InputError
			if err := validateRowKey(key, primaryKeys); !errors.As(err, &inputErr) {
				t.Fatalf("key %v seharusnya ditolak, err=%v", key, err)
			}
		})
	}
}

func TestValidateEditableColumn(t *testing.T) {
	columns := map[string]editableColumn{
		"regular":  {},
		"identity": {generated: true},
	}
	if err := validateEditableColumn("regular", columns); err != nil {
		t.Fatalf("kolom biasa ditolak: %v", err)
	}
	for _, name := range []string{"missing", "identity"} {
		var inputErr InputError
		if err := validateEditableColumn(name, columns); !errors.As(err, &inputErr) {
			t.Errorf("kolom %q seharusnya ditolak, err=%v", name, err)
		}
	}
}
