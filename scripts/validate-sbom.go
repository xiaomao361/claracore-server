package main

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

const (
	wantBOMFormat   = "CycloneDX"
	wantSpecVersion = "1.6"
	wantToolName    = "cyclonedx-gomod"
	wantToolVersion = "v1.10.0"
)

type bom struct {
	BOMFormat    string        `json:"bomFormat"`
	SpecVersion  string        `json:"specVersion"`
	SerialNumber string        `json:"serialNumber"`
	Metadata     metadata      `json:"metadata"`
	Components   []component   `json:"components"`
	Dependencies []dependency  `json:"dependencies"`
	Compositions []composition `json:"compositions"`
}

type metadata struct {
	Timestamp  string     `json:"timestamp"`
	Tools      []tool     `json:"tools"`
	Component  component  `json:"component"`
	Properties []property `json:"properties"`
}

type tool struct {
	Vendor  string `json:"vendor"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type component struct {
	BOMRef  string `json:"bom-ref"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version"`
	PURL    string `json:"purl"`
	Hashes  []hash `json:"hashes"`
}

type hash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}

type property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

type composition struct {
	Aggregate    string   `json:"aggregate"`
	Dependencies []string `json:"dependencies"`
}

func main() {
	var sbomPath, binaryPath, version, arch string
	flag.StringVar(&sbomPath, "sbom", "", "CycloneDX JSON path")
	flag.StringVar(&binaryPath, "binary", "", "Go binary path")
	flag.StringVar(&version, "version", "", "release version")
	flag.StringVar(&arch, "arch", "", "target architecture")
	flag.Parse()
	if err := validate(sbomPath, binaryPath, version, arch); err != nil {
		fmt.Fprintln(os.Stderr, "SBOM 验证失败:", err)
		os.Exit(1)
	}
}

func validate(sbomPath, binaryPath, version, arch string) error {
	if sbomPath == "" || binaryPath == "" || version == "" || (arch != "amd64" && arch != "arm64") {
		return errors.New("必须提供 SBOM、二进制、版本和受支持的 Linux 架构")
	}
	body, err := os.ReadFile(sbomPath)
	if err != nil {
		return fmt.Errorf("读取 SBOM: %w", err)
	}
	if strings.Contains(string(body), "/Users/") || strings.Contains(string(body), "/tmp/") {
		return errors.New("SBOM 泄露构建机绝对路径")
	}
	var document bom
	if err := json.Unmarshal(body, &document); err != nil {
		return fmt.Errorf("解析 JSON: %w", err)
	}
	if document.BOMFormat != wantBOMFormat || document.SpecVersion != wantSpecVersion {
		return fmt.Errorf("格式为 %s/%s，要求 %s/%s", document.BOMFormat, document.SpecVersion, wantBOMFormat, wantSpecVersion)
	}
	if document.SerialNumber != "" || document.Metadata.Timestamp != "" {
		return errors.New("发布 SBOM 必须省略随机 serialNumber 与生成时间")
	}
	if len(document.Metadata.Tools) != 1 || document.Metadata.Tools[0].Vendor != "CycloneDX" ||
		document.Metadata.Tools[0].Name != wantToolName || document.Metadata.Tools[0].Version != wantToolVersion {
		return errors.New("SBOM 生成器身份或版本不符合固定门禁")
	}
	main := document.Metadata.Component
	if main.Type != "application" || main.Name != "claracore-server" || main.Version != version {
		return fmt.Errorf("主组件身份不匹配: type=%s name=%s version=%s", main.Type, main.Name, main.Version)
	}
	if err := validatePURL(main.PURL, arch); err != nil {
		return fmt.Errorf("主组件 PURL: %w", err)
	}

	info, err := buildinfo.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("读取 Go 构建信息: %w", err)
	}
	binaryBody, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("读取二进制: %w", err)
	}
	binaryHash := sha256.Sum256(binaryBody)
	binarySHA256 := hex.EncodeToString(binaryHash[:])
	properties := make(map[string]string, len(document.Metadata.Properties))
	for _, item := range document.Metadata.Properties {
		properties[item.Name] = item.Value
	}
	wantProperties := map[string]string{
		"cdx:gomod:binary:hash:SHA-256":   binarySHA256,
		"cdx:gomod:binary:name":           "claracore-server",
		"cdx:gomod:build:env:CGO_ENABLED": "0",
		"cdx:gomod:build:env:GOARCH":      arch,
		"cdx:gomod:build:env:GOOS":        "linux",
		"cdx:gomod:build:env:GOVERSION":   info.GoVersion,
	}
	for name, want := range wantProperties {
		if properties[name] != want {
			return fmt.Errorf("属性 %s=%q，要求 %q", name, properties[name], want)
		}
	}

	expected := make(map[string]string, len(info.Deps)+1)
	for _, dep := range info.Deps {
		module := dep
		if dep.Replace != nil {
			module = dep.Replace
		}
		if module.Path == "" || module.Version == "" {
			return fmt.Errorf("依赖缺少可追踪版本: %s", dep.Path)
		}
		if _, exists := expected[module.Path]; exists {
			return fmt.Errorf("依赖路径重复: %s", module.Path)
		}
		expected[module.Path] = module.Version
	}
	expected["std"] = info.GoVersion
	if len(document.Components) != len(expected) {
		return fmt.Errorf("组件数为 %d，按二进制构建信息应为 %d", len(document.Components), len(expected))
	}
	componentRefs := make(map[string]bool, len(document.Components))
	linkedModuleRefs := make(map[string]bool, len(info.Deps))
	for _, item := range document.Components {
		wantVersion, exists := expected[item.Name]
		if !exists || item.Version != wantVersion {
			return fmt.Errorf("组件不在二进制依赖集或版本不符: %s@%s", item.Name, item.Version)
		}
		delete(expected, item.Name)
		if item.BOMRef == "" || componentRefs[item.BOMRef] {
			return fmt.Errorf("组件 bom-ref 缺失或重复: %s", item.Name)
		}
		componentRefs[item.BOMRef] = true
		if item.Name != "std" {
			linkedModuleRefs[item.BOMRef] = true
		}
		if err := validatePURL(item.PURL, arch); err != nil {
			return fmt.Errorf("组件 %s PURL: %w", item.Name, err)
		}
		if !hasSHA256(item.Hashes) && item.Name != "std" {
			return fmt.Errorf("组件缺少 SHA-256: %s", item.Name)
		}
	}
	if len(expected) != 0 {
		missing := make([]string, 0, len(expected))
		for name := range expected {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf("SBOM 缺少二进制依赖: %s", strings.Join(missing, ", "))
	}

	var root *dependency
	for i := range document.Dependencies {
		if document.Dependencies[i].Ref == main.BOMRef {
			root = &document.Dependencies[i]
			break
		}
	}
	if root == nil {
		return errors.New("SBOM 缺少主组件依赖图")
	}
	rootRefs := make(map[string]bool, len(root.DependsOn))
	for _, ref := range root.DependsOn {
		rootRefs[ref] = true
	}
	if len(rootRefs) != len(linkedModuleRefs) {
		return fmt.Errorf("主组件依赖图有 %d 项，按二进制构建信息应为 %d", len(rootRefs), len(linkedModuleRefs))
	}
	for ref := range linkedModuleRefs {
		if !rootRefs[ref] {
			return fmt.Errorf("主组件依赖图缺少 %s", ref)
		}
	}
	var completeRoot, componentSet bool
	for _, item := range document.Compositions {
		refs := make(map[string]bool, len(item.Dependencies))
		for _, ref := range item.Dependencies {
			refs[ref] = true
		}
		if item.Aggregate == "complete" && len(refs) == 1 && refs[main.BOMRef] {
			completeRoot = true
		}
		if item.Aggregate == "unknown" && len(refs) == len(componentRefs) {
			componentSet = true
			for ref := range componentRefs {
				if !refs[ref] {
					componentSet = false
					break
				}
			}
		}
	}
	if !completeRoot || !componentSet {
		return errors.New("SBOM composition 未完整绑定主组件、链接模块与标准库组件集")
	}

	fmt.Printf("tool=cyclonedx-gomod@v1.10.0 format=CycloneDX@1.6 target=linux/%s\n", arch)
	fmt.Println("result=verified")
	fmt.Printf("components=%d linked_modules=%d standard_library=%s\n", len(document.Components), len(info.Deps), info.GoVersion)
	fmt.Printf("binary_sha256=%s\n", binarySHA256)
	return nil
}

func validatePURL(purl, arch string) error {
	question := strings.IndexByte(purl, '?')
	if question < 0 {
		return errors.New("缺少目标平台 qualifiers")
	}
	fragment := strings.IndexByte(purl[question+1:], '#')
	query := purl[question+1:]
	if fragment >= 0 {
		query = query[:fragment]
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return fmt.Errorf("解析 qualifiers: %w", err)
	}
	if values.Get("goos") != "linux" || values.Get("goarch") != arch {
		return fmt.Errorf("目标为 %s/%s，要求 linux/%s", values.Get("goos"), values.Get("goarch"), arch)
	}
	return nil
}

func hasSHA256(hashes []hash) bool {
	for _, item := range hashes {
		if item.Algorithm == "SHA-256" && len(item.Content) == sha256.Size*2 {
			if _, err := hex.DecodeString(item.Content); err == nil {
				return true
			}
		}
	}
	return false
}
