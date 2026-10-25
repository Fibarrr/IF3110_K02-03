package migration

import "testing"

func TestValidateInput(t *testing.T) {
	tests := []struct {
		name    string
		input   Input
		wantErr bool
	}{
		{name: "valid", input: Input{Name: "Create table", SQL: "CREATE TABLE items (id INT)"}},
		{name: "empty name", input: Input{SQL: "SELECT 1"}, wantErr: true},
		{name: "empty SQL", input: Input{Name: "Empty"}, wantErr: true},
		{name: "name too long", input: Input{Name: string(make([]byte, 121)), SQL: "SELECT 1"}, wantErr: true},
		{name: "SQL too long", input: Input{Name: "Large", SQL: string(make([]byte, maxSQLBytes+1))}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateInput(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateInput() error = %v, mau error %v", err, test.wantErr)
			}
		})
	}
}
