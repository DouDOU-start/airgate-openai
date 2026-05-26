package gateway

import (
	"fmt"
	"strings"
)

const (
	gptImageTokenFormulaBias  = int64(2_000_000)
	gptImageTokenFormulaScale = int64(4_000_000)
	gptImageDefaultQuality    = "high"
)

// GPTImageTokenCalculator mirrors OpenAI's GPT Image 2 token calculator:
// size + low/medium/high quality -> estimated image tokens.
type GPTImageTokenCalculator struct{}

// Calculate parses size as WIDTHxHEIGHT and returns GPT Image 2 image tokens for one image.
func (GPTImageTokenCalculator) Calculate(size, quality string) (int, error) {
	width, height, ok := parseImageSize(size)
	if !ok {
		return 0, fmt.Errorf("size 格式无效，应为 WIDTHxHEIGHT")
	}
	return calculateGPTImageTokens(width, height, quality)
}

// CalculateDefaultQuality parses size and calculates tokens with the default high quality.
func (c GPTImageTokenCalculator) CalculateDefaultQuality(size string) (int, error) {
	return c.Calculate(size, gptImageDefaultQuality)
}

// CalculateDimensions returns GPT Image 2 image tokens for one image.
func (GPTImageTokenCalculator) CalculateDimensions(width, height int, quality string) (int, error) {
	return calculateGPTImageTokens(width, height, quality)
}

// CalculateDimensionsDefaultQuality calculates tokens with the default high quality.
func (c GPTImageTokenCalculator) CalculateDimensionsDefaultQuality(width, height int) (int, error) {
	return c.CalculateDimensions(width, height, gptImageDefaultQuality)
}

func calculateGPTImageTokens(width, height int, quality string) (int, error) {
	if width <= 0 || height <= 0 {
		return 0, fmt.Errorf("size 宽高必须大于 0")
	}
	base, err := gptImageQualityBase(quality)
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
	return int(ceilPositiveRatio(patches*(gptImageTokenFormulaBias+area), gptImageTokenFormulaScale)), nil
}

func gptImageQualityBase(quality string) (int, error) {
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
