package workspace

import (
	"path"
	"strings"
)

// generatedDirNames 是各语言工具链生成的目录名。这些目录永远不该进 PR：
// 验证命令（如 `python -m unittest`）会在工作区里生成它们，如果不排除，
// `git add -A` 就会把 __pycache__ 之类的垃圾一起提交。
var generatedDirNames = map[string]bool{
	"__pycache__":   true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
	".tox":          true,
	"node_modules":  true,
	".venv":         true,
	"venv":          true,
	".gradle":       true,
	".idea":         true,
	".DS_Store":     true,
}

// generatedExts 是编译产物后缀。
var generatedExts = map[string]bool{
	".pyc": true, ".pyo": true, ".pyd": true,
	".class": true, ".o": true, ".obj": true,
}

// IsGeneratedArtifact 报告相对路径按 POSIX 分隔时是否属于工具生成的产物。
//
// 判断分两步：路径中的任意一段命中生成目录名（说明文件在这些目录内），
// 或者文件名后缀属于编译产物。判定刻意保守——只覆盖“任何仓库都不该提交”
// 的类别，避免把用户真正想提交的新文件挡在提交之外。
func IsGeneratedArtifact(rel string) bool {
	rel = path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "." || rel == "" {
		return false
	}
	segments := strings.Split(rel, "/")
	for _, seg := range segments {
		if generatedDirNames[seg] {
			return true
		}
	}
	return generatedExts[path.Ext(segments[len(segments)-1])]
}
