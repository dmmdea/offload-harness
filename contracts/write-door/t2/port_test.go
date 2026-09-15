package port

import "testing"

func TestParsePort(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{name: "http", in: "80", want: 80},
		{name: "high", in: "65535", want: 65535},
		{name: "zero", in: "0", wantErr: true},
		{name: "text", in: "abc", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParsePort(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: ParsePort(%q) = %d, want an error", tc.name, tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: ParsePort(%q) returned %v", tc.name, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: ParsePort(%q) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}
