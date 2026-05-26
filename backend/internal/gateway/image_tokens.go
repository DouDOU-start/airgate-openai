package gateway

import (
	"fmt"
	"strings"
)

const (
	gptImage2TokenFormulaBias  = int64(2_000_000)
	gptImage2TokenFormulaScale = int64(4_000_000)
)

// GPTImage2TokenCalculator mirrors OpenAI's GPT Image 2 token calculator:
// size + low/medium/high quality -> estimated image tokens.
type GPTImage2TokenCalculator struct{}

// NewGPTImage2TokenCalculator returns a stateless GPT Image 2 token calculator.
func NewGPTImage2TokenCalculator() GPTImage2TokenCalculator {
	return GPTImage2TokenCalculator{}
}

// Calculate parses size as WIDTHxHEIGHT and returns GPT Image 2 image tokens for one image.
func (GPTImage2TokenCalculator) Calculate(size, quality string) (int, error) {
	width, height, ok := parseImageSize(size)
	if !ok {
		return 0, fmt.Errorf("size 格式无效，应为 WIDTHxHEIGHT")
	}
	return calculateGPTImage2Tokens(width, height, quality)
}

// CalculateDimensions returns GPT Image 2 image tokens for one image.
func (GPTImage2TokenCalculator) CalculateDimensions(width, height int, quality string) (int, error) {
	return calculateGPTImage2Tokens(width, height, quality)
}

func calculateGPTImage2Tokens(width, height int, quality string) (int, error) {
	if width <= 0 || height <= 0 {
		return 0, fmt.Errorf("size 宽高必须大于 0")
	}
	base, err := gptImage2QualityBase(quality)
	if err != nil {
		return 0, err
	}

	longEdge, shortEdge := width, height
	if shortEdge > longEdge {
		longEdge, shortEdge = shortEdge, longEdge
	}

	scaledShort := roundPositiveRatio(int64(base*shortEdge), int64(longEdge))
	patches := int64(base) * scaledShort
	area := int64(width) * int64(height)
	return int(ceilPositiveRatio(patches*(gptImage2TokenFormulaBias+area), gptImage2TokenFormulaScale)), nil
}

func gptImage2QualityBase(quality string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low":
		return 16, nil
	case "medium":
		return 48, nil
	case "high":
		return 96, nil
	default:
		return 0, fmt.Errorf("quality 必须是 low、medium 或 high")
	}
}

func roundPositiveRatio(numerator, denominator int64) int64 {
	return (numerator*2 + denominator) / (denominator * 2)
}

func ceilPositiveRatio(numerator, denominator int64) int64 {
	return (numerator + denominator - 1) / denominator
}
