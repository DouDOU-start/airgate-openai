// 嵌入静态资源，供其他包引用
package resources

import _ "embed"

//go:embed instructions.md
var defaultInstructions string

//go:embed instructions-simple.md
var simpleInstructions string

//go:embed instructions-nsfw.md
var nsfwInstructions string

// DefaultInstructions 是完整版本的系统提示词。
var DefaultInstructions = defaultInstructions

// SimpleInstructions 是当前默认使用的精简版本系统提示词。
var SimpleInstructions = simpleInstructions

// NsfwInstructions 是 NSFW 版本的系统提示词。
var NsfwInstructions = nsfwInstructions

// Instructions 是当前使用的系统提示词，切换时只需修改此处
var Instructions = NsfwInstructions

// ResolveInstructions 根据名称解析 instructions 内容。
// 支持内置别名 "default" / "simple" / "nsfw"，其他值原样返回（视为完整 instructions 文本）。
func ResolveInstructions(name string) string {
	switch name {
	case "default":
		return DefaultInstructions
	case "simple":
		return SimpleInstructions
	case "nsfw":
		return NsfwInstructions
	default:
		return name
	}
}
