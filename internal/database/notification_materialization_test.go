package database

import "testing"

func TestValidateMaterializedPositions(t *testing.T) {
	one, two := int16(1), int16(2)
	tests := []struct {
		name      string
		version   int16
		positions []*int16
		legacy    bool
		wantError bool
	}{
		{name: "legacy accepts missing positions", version: 1, positions: []*int16{nil, nil}, legacy: true},
		{name: "legacy rejects empty report set", version: 1, positions: nil, wantError: true},
		{name: "v2 accepts contiguous positions", version: 2, positions: []*int16{&one, &two}},
		{name: "v2 rejects missing position", version: 2, positions: []*int16{&one, nil}, wantError: true},
		{name: "v2 rejects gaps", version: 2, positions: []*int16{&one, func() *int16 { value := int16(3); return &value }()}, wantError: true},
		{name: "v2 rejects duplicate positions", version: 2, positions: []*int16{&one, &one}, wantError: true},
		{name: "unknown version is rejected", version: 3, positions: []*int16{&one}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacy, err := validateMaterializedPositions(test.version, test.positions)
			if (err != nil) != test.wantError || legacy != test.legacy {
				t.Fatalf("validateMaterializedPositions() legacy=%v err=%v", legacy, err)
			}
		})
	}
}
