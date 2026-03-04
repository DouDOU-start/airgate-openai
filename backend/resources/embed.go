// 嵌入静态资源，供其他包引用
package resources

import _ "embed"

//go:embed instructions.md
var defaultInstructions string

//go:embed instructions-simple.md
var simpleInstructions string

// Instructions 是当前使用的系统提示词，切换时只需修改此处
var Instructions = simpleInstructions
