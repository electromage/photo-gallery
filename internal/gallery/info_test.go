package gallery

import "testing"

func TestFormatExposure(t *testing.T) {
	cases := map[float64]string{
		0:     "",
		0.004: "1/250 s",
		0.5:   "1/2 s",
		1:     "1 s",
		1.5:   "1.5 s",
		2:     "2 s",
		30:    "30 s",
	}
	for in, want := range cases {
		if got := formatExposure(in); got != want {
			t.Errorf("formatExposure(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestCameraName(t *testing.T) {
	cases := []struct{ make, model, want string }{
		{"NIKON CORPORATION", "NIKON D750", "NIKON D750"},
		{"Canon", "Canon EOS 5D", "Canon EOS 5D"},
		{"SONY", "ILCE-7M3", "SONY ILCE-7M3"},
		{"", "iPhone 15 Pro", "iPhone 15 Pro"},
		{"FUJIFILM", "", "FUJIFILM"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := cameraName(c.make, c.model); got != c.want {
			t.Errorf("cameraName(%q, %q) = %q, want %q", c.make, c.model, got, c.want)
		}
	}
}

func TestBuildInfoDimensions(t *testing.T) {
	info := buildInfo(nil, 640, 480)
	if len(info) != 1 || info[0].Label != "Dimensions" || info[0].Value != "640 × 480" {
		t.Fatalf("buildInfo(nil,640,480) = %+v, want single Dimensions 640 × 480", info)
	}

	if got := buildInfo(nil, 0, 0); len(got) != 0 {
		t.Errorf("buildInfo with no dimensions = %+v, want empty", got)
	}
}
