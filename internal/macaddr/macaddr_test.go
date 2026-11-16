package macaddr

import "testing"

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"AA:BB:CC:DD:EE:FF":  "aa:bb:cc:dd:ee:ff",
		"aa-bb-cc-dd-ee-ff":  "aa:bb:cc:dd:ee:ff",
		"aabb.ccdd.eeff":     "aa:bb:cc:dd:ee:ff",
		" 02:00:00:00:00:01": "02:00:00:00:00:01",
	} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "zz:bb:cc:dd:ee:ff", "00:00:5e:10:00:00:00:01"} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) succeeded, want error", bad)
		}
	}
}

func TestFromBytes(t *testing.T) {
	if got, ok := FromBytes([]byte{0xde, 0xad, 0xbe, 0xef, 0, 1}); !ok || got != "de:ad:be:ef:00:01" {
		t.Fatalf("FromBytes = %q %v", got, ok)
	}
	if _, ok := FromBytes([]byte{1, 2}); ok {
		t.Fatal("FromBytes accepted short input")
	}
}

func TestIsLocallyAdministered(t *testing.T) {
	if !IsLocallyAdministered("da:a1:19:00:00:01") {
		t.Error("expected randomized MAC to be locally administered")
	}
	if IsLocallyAdministered("00:0c:42:00:00:01") {
		t.Error("expected MikroTik OUI to be universally administered")
	}
}
