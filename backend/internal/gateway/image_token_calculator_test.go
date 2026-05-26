package gateway

import (
	"strings"
	"testing"
)

func TestGPTImageTokenCalculator(t *testing.T) {
	calc := GPTImageTokenCalculator{}
	cases := []struct {
		size    string
		quality string
		want    int
	}{
		{"1024x1024", "low", 196},
		{"1024x1024", "medium", 1756},
		{"1024x1024", "high", 7024},
		{"1024x1536", "low", 158},
		{"1536x1024", "medium", 1372},
		{"3840x2160", "low", 371},
		{"3840x2160", "medium", 3336},
		{"3840x2160", "high", 13342},
		{"3840X2160", "HIGH", 13342},
	}

	for _, tc := range cases {
		t.Run(tc.size+"_"+tc.quality, func(t *testing.T) {
			got, err := calc.Calculate(tc.size, tc.quality)
			if err != nil {
				t.Fatalf("Calculate(%q, %q) returned err: %v", tc.size, tc.quality, err)
			}
			if got != tc.want {
				t.Fatalf("Calculate(%q, %q) = %d, want %d", tc.size, tc.quality, got, tc.want)
			}
		})
	}
}

func TestGPTImageTokenCalculatorRejectsUnparseableInput(t *testing.T) {
	calc := GPTImageTokenCalculator{}
	cases := []struct {
		name       string
		size       string
		quality    string
		wantSubstr string
	}{
		{"auto size", "auto", "low", "WIDTHxHEIGHT"},
		{"bad size", "1024", "low", "WIDTHxHEIGHT"},
		{"unknown quality", "1024x1024", "standard", "quality"},
		{"zero dimensions", "0x1024", "low", "WIDTHxHEIGHT"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := calc.Calculate(tc.size, tc.quality)
			if err == nil {
				t.Fatalf("Calculate(%q, %q) = nil err, want invalid input error", tc.size, tc.quality)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestGPTImageTokenCalculatorDefaultQualityUsesHigh(t *testing.T) {
	calc := GPTImageTokenCalculator{}

	got, err := calc.CalculateDefaultQuality("3840x2160")
	if err != nil {
		t.Fatalf("CalculateDefaultQuality returned err: %v", err)
	}
	if got != 13342 {
		t.Fatalf("CalculateDefaultQuality = %d, want high-quality 13342", got)
	}

	got, err = calc.CalculateDimensionsDefaultQuality(1024, 1024)
	if err != nil {
		t.Fatalf("CalculateDimensionsDefaultQuality returned err: %v", err)
	}
	if got != 7024 {
		t.Fatalf("CalculateDimensionsDefaultQuality = %d, want high-quality 7024", got)
	}
}

func TestGPTImageTokenCalculatorDoesNotValidateSizeRules(t *testing.T) {
	got, err := GPTImageTokenCalculator{}.Calculate("512x512", "low")
	if err != nil {
		t.Fatalf("Calculate returned err: %v", err)
	}
	if got != 145 {
		t.Fatalf("Calculate = %d, want 145", got)
	}
}

func TestGPTImageTokenCalculatorDimensions(t *testing.T) {
	got, err := GPTImageTokenCalculator{}.CalculateDimensions(3840, 2160, "medium")
	if err != nil {
		t.Fatalf("CalculateDimensions returned err: %v", err)
	}
	if got != 3336 {
		t.Fatalf("CalculateDimensions = %d, want 3336", got)
	}
}
