package gazelle_haskell_modules

import (
	"encoding/json"
	"fmt"
	"log"
	"path"
	"path/filepath"

	"github.com/bazelbuild/buildtools/build"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/rule"
	"github.com/bazelbuild/rules_go/go/tools/bazel"

	"os/exec"
	"strings"
)

// Extracts the source files from Haskell rules and creates
// haskell_module rules to build them.
//
// For existing haskell_module rules, it also sets some private attributes as a side effect
// which are needed when indexing the rule.
func rulesToRuleInfos(pkgRoot string, rules []*rule.Rule, repo string, pkg string) []*RuleInfo {
	ruleInfoss0, originatingRules := nonHaskellModuleRulesToRuleInfos(pkgRoot, rules, repo, pkg)
	ruleInfoss1 := haskellModuleRulesToRuleInfos(pkgRoot, rules, repo, pkg, originatingRules)
	return concatRuleInfos(append(ruleInfoss0, ruleInfoss1...))
}

const PRIVATE_ATTR_MODULE_LABELS = "module_labels"
const PRIVATE_ATTR_DEP_LABELS = "dep_labels"
const PRIVATE_ATTR_MODULE_NAME = "module_name"
const PRIVATE_ATTR_ORIGINATING_RULE = "originating_rule"

// Yields the rule infos and a map of module labels to the rules that
// enclose the modules.
func nonHaskellModuleRulesToRuleInfos(
	pkgRoot string,
	rules []*rule.Rule,
	repo string,
	pkg string,
) ([][]*RuleInfo, map[label.Label][]*rule.Rule) {
	ruleInfoss := make([][]*RuleInfo, 0, 100)
	originatingRules := make(map[label.Label][]*rule.Rule, 100)
	// Analyze non-haskell_module rules
	for _, r := range rules {
		if !isNonHaskellModule(r.Kind()) || !shouldModularize(r) {
			continue
		}
		srcs, err := srcsFromRule(pkgRoot, r.Attr("srcs"))
		handleRuleError(err, r, "srcs")

		modules, err := depsFromRule(r.Attr("modules"), repo, pkg)
		handleRuleError(err, r, "modules")

		modDatas := haskellModulesToModuleData(srcs)
		ruleInfos := make([]*RuleInfo, len(modDatas))
		moduleLabels := make(map[label.Label]bool, len(modules) + len(srcs))
		for i, modData := range modDatas {
			ruleInfos[i] = &RuleInfo {
				OriginatingRules: []*rule.Rule{r},
				ModuleData: modData,
			}
			modLabel := label.New(repo, pkg, ruleNameFromRuleInfo(ruleInfos[i]))
			addOriginatingRule(originatingRules, &modLabel, r)
			moduleLabels[modLabel] = true
		}
		ruleInfoss = append(ruleInfoss, ruleInfos)

		for mod, _ := range modules {
			addOriginatingRule(originatingRules, &mod, r)
			moduleLabels[mod] = true
		}

		r.SetPrivateAttr(PRIVATE_ATTR_MODULE_LABELS, moduleLabels)
	}
	return ruleInfoss, originatingRules
}

// Adds a rule to a map at the given label.
func addOriginatingRule(originatingRules map[label.Label][]*rule.Rule, mod *label.Label, r *rule.Rule) {
	oRules := originatingRules[*mod]
	if oRules == nil {
		originatingRules[*mod] = []*rule.Rule{r}
	} else {
		originatingRules[*mod] = append(oRules, r)
	}
}

// originatingRules is used to determine which rule is originating a haskell_module
// rule, which is used in turn to determine which modules from the same originating
// rule are meant in imports.
func haskellModuleRulesToRuleInfos(
	pkgRoot string,
	rules []*rule.Rule,
	repo string,
	pkg string,
	originatingRules map[label.Label][]*rule.Rule,
) [][]*RuleInfo {
	ruleInfoss := make([][]*RuleInfo, 0, 100)
	// Analyze haskell_module rules
	for _, r := range rules {
		if r.Kind() != "haskell_module" {
			continue
		}

		src := path.Join(pkgRoot, r.AttrString("src"))

		rLabel := label.New(repo, pkg, r.Name())
		oRules, ok := originatingRules[rLabel]
		if !ok {
			continue
		}

		modDatas := haskellModulesToModuleData([]string{src})
		ruleInfo := RuleInfo {
			OriginatingRules: oRules,
			ModuleData: modDatas[0],
		}

		ruleInfoss = append(ruleInfoss, []*RuleInfo{&ruleInfo})

		r.SetPrivateAttr(PRIVATE_ATTR_MODULE_NAME, ruleInfo.ModuleData.ModuleName)
		r.SetPrivateAttr(PRIVATE_ATTR_ORIGINATING_RULE, ruleInfo.OriginatingRules)
	}
	return ruleInfoss
}

const HIMPORTSCAN_PATH = "himportscan/himportscan"

// Collects the imported modules from every sourcefile
//
// Module file paths must be absolute.
func haskellModulesToModuleData(moduleFiles []string) []*ModuleData {
	himportscan, err := bazel.Runfile(HIMPORTSCAN_PATH)
	if err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command(himportscan)

	cmd.Stdin = strings.NewReader(strings.Join(moduleFiles, "\n"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("%s", out)
		log.Fatal(err)
	}
	var modDatas []*ModuleData
	err = json.Unmarshal(out, &modDatas)
	if err != nil {
		log.Printf("Incorrect json: %s\n", out)
		log.Fatal(err)
	}
	return modDatas
}

func infoToRules(pkgRoot string, ruleInfos []*RuleInfo) language.GenerateResult {

	theRules := make([]*rule.Rule, len(ruleInfos))
	theImports := make([]interface{}, len(ruleInfos))
	for i, ruleInfo := range ruleInfos {
		ruleName := ruleNameFromRuleInfo(ruleInfo)
		r := rule.NewRule("haskell_module", ruleName)
		r.SetPrivateAttr(PRIVATE_ATTR_MODULE_NAME, ruleInfo.ModuleData.ModuleName)
		r.SetPrivateAttr(PRIVATE_ATTR_ORIGINATING_RULE, ruleInfo.OriginatingRules)
		file, _ := filepath.Rel(pkgRoot, ruleInfo.ModuleData.FilePath)
		r.SetAttr("src", file)
		stripPrefix := srcStripPrefix(file, ruleInfo.ModuleData.ModuleName)
		r.SetAttr("src_strip_prefix", stripPrefix)
		idealModFile := fmt.Sprintf("%s/%s%s", stripPrefix, strings.Replace(ruleInfo.ModuleData.ModuleName, ".", "/", -1), filepath.Ext(file))
		if file != idealModFile {
			r.SetAttr("module_name", ruleInfo.ModuleData.ModuleName)
		}
		r.AddComment("# rule generated by gazelle_haskell_modules")

		theRules[i] = r
		theImports[i] = &HModuleImportData {
			ImportedModules: ruleInfo.ModuleData.ImportedModules,
			UsesTH: ruleInfo.ModuleData.UsesTH,
		}
	}

	return language.GenerateResult{
		Gen:     theRules,
		Imports: theImports,
	}
}

func addNonHaskellModuleRules(
	c *Config,
	pkgRoot string,
	repo string,
	pkg string,
	gen language.GenerateResult,
	rules []*rule.Rule,
) language.GenerateResult {
	haskellRules := make([]*rule.Rule, 0, len(rules))
	imports := make([]interface{}, 0, len(rules))
	for _, r := range rules {
		if !shouldModularize(r) {
			continue
		}
		if isNonHaskellModule(r.Kind()) {
			newr := rule.NewRule(r.Kind(), r.Name())
			for _, k := range r.AttrKeys() {
				if k != "srcs" && k != "modules" && k != "deps" && k != "narrowed_deps" && k != "main_file" {
					newr.SetAttr(k, r.Attr(k))
				}
			}

			srcs, err := srcsFromRule(pkgRoot, r.Attr("srcs"))
			handleRuleError(err, r, "srcs")
			modules, err := depsFromRule(r.Attr("modules"), repo, pkg)
			handleRuleError(err, r, "modules")
			deps, err := depsFromRule(r.Attr("deps"), repo, pkg)
			handleRuleError(err, r, "deps")
			narrowedDeps, err := depsFromRule(r.Attr("narrowed_deps"), repo, pkg)
			handleRuleError(err, r, "narrowed_deps")
			appendLabelMaps(deps, narrowedDeps)
			imports = append(imports, &HRuleImportData {
				Deps: deps,
				Modules: modules,
				Srcs: srcs,
			})
			haskellRules = append(haskellRules, newr)

			r.SetPrivateAttr(PRIVATE_ATTR_DEP_LABELS, deps)
			newr.SetPrivateAttr(PRIVATE_ATTR_DEP_LABELS, deps)
			newr.SetPrivateAttr(PRIVATE_ATTR_MODULE_LABELS, r.PrivateAttr(PRIVATE_ATTR_MODULE_LABELS))
		}
	}
	return language.GenerateResult{
		Gen:     append(gen.Gen, haskellRules...),
		Imports: append(gen.Imports, imports...),
	}
}

func appendLabelMaps(a map[label.Label]bool, b map[label.Label]bool) {
	for k, v := range b {
		a[k] = v
	}
}

func handleRuleError(err error, r *rule.Rule, attr string) {
	if err != nil {
		fmt.Println("Error when analyzing target", r.Name())
		fmt.Println(attr, "=", build.FormatString(r.Attr(attr)))
		log.Fatal(err)
	}
}

func concatRuleInfos(xs [][]*RuleInfo) []*RuleInfo {
	s := 0
	for _, x := range xs {
		s += len(x)
	}
	ys := make([]*RuleInfo, s)
	i := 0
	for _, x := range xs {
		for _, y := range x {
			ys[i] = y
			i++
		}
	}
	return ys
}

// Collects the source files referenced in the given expression
func srcsFromRule(pkgRoot string, expr build.Expr) ([]string, error) {
	srcs, err := getSources(expr)
	if err != nil {
		return nil, err
	}

	xs := make([]string, len(srcs))
	i := 0
	for f, _ := range srcs {
		xs[i] = path.Join(pkgRoot, f)
		i++
	}

	return xs, nil
}

// Collects the dependencies referenced in the given expression
func depsFromRule(expr build.Expr, repo string, pkg string) (map[label.Label]bool, error) {
	deps, err := getLabelsFromListExpr(expr)
	if err != nil {
		return nil, err
	}

	xs := make(map[label.Label]bool, len(deps))
	for lbl, _ := range deps {
		xs[abs(lbl, repo, pkg)] = true
	}

	return xs, nil
}

func setVisibilities(f *rule.File, rules []*rule.Rule) {
	if f == nil || !f.HasDefaultVisibility() {
		for _, r := range rules {
			r.SetAttr("visibility", []string{"//visibility:public"})
		}
	}
}

type ModuleData struct {
	ModuleName string
	FilePath  string
	ImportedModules []ModuleImport
	UsesTH bool
}

type ModuleImport struct {
	PackageName string
	ModuleName string
}

func (moduleImport *ModuleImport) UnmarshalJSON(data []byte) error {
	var aux []string
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	pkgName := ""
	if len(aux) > 1 {
		pkgName = aux[0]
	}
	moduleImport.PackageName = pkgName
	moduleImport.ModuleName = aux[len(aux)-1]
	return nil
}

type RuleInfo struct {
	OriginatingRules []*rule.Rule
	ModuleData *ModuleData
}

type HModuleImportData struct {
	ImportedModules []ModuleImport
	UsesTH bool
}

type HRuleImportData struct {
	Deps map[label.Label]bool // Absolute labels of deps of the library/binary/test
	Modules map[label.Label]bool // Absolute labels of the modules in the library, empty if not a library
	Srcs []string
}

func getLabelsFromListExpr(expr build.Expr) (map[label.Label]bool, error) {
	switch expr.(type) {
	case nil:
		return map[label.Label]bool{}, nil
	case *build.ListExpr:
		exprList := expr.(*build.ListExpr).List
		xs := make(map[label.Label]bool, len(exprList))
		for _, e := range exprList {
			switch e.(type) {
			case *build.StringExpr:
				lbl, err := ParseLabel(e.(*build.StringExpr).Value)
				if err != nil {
					return nil, err
				}
				xs[lbl] = true
			default:
				return nil, fmt.Errorf("Unhandled expression type %T (expected a string)", e)
			}
		}
		return xs, nil
	default:
		return nil, fmt.Errorf("Unhandled expression type %T (expected a list)", expr)
	}
}

// We use a patched parsing for labels, as the parser from gazelle can't understand
// labels of the form "@repo", supposed to mean "@repo//:repo".
func ParseLabel(v string) (label.Label, error) {
	if strings.HasPrefix(v, "@") && !strings.Contains(v, "//") && !strings.Contains(v, ":") {
		v = fmt.Sprintf("%s//:%s",v, v[1:])
	}
	return label.Parse(v)
}

func getSources(expr build.Expr) (map[string]bool, error) {
	xs, err := getStringList(expr)
	if err != nil {
		return nil, err
	}
	sourceMap := make(map[string]bool, len(xs))
	for _, x := range xs {
		sourceMap[x] = true
	}
	return sourceMap, nil
}

// Similar to (*Rule) AttrStrings(key string) []string
// but yields an empty list if the attribute isn't set, and
// gives an error if the attribute is set to something that
// isn't a list.
func getStringList(expr build.Expr) ([]string, error) {
	switch expr.(type) {
	case nil:
		return []string{}, nil
	case *build.ListExpr:
		exprList := expr.(*build.ListExpr).List
		xs := make([]string, 0, len(exprList))
		for _, e := range exprList {
			switch e.(type) {
			case *build.StringExpr:
				estr := e.(*build.StringExpr)
				xs = append(xs, estr.Value)
			default:
				return nil, fmt.Errorf("Unhandled expression type %T (expected a string)", e)
			}
		}
		return xs, nil
	default:
		return nil, fmt.Errorf("Unhandled expression type %T (expected a list)", expr)
	}
}

func isNonHaskellModule(kind string) bool {
	return kind == "haskell_library" ||
		kind == "haskell_binary" ||
		kind == "haskell_test"
}

// Computes the prefix of file that doesn't correspond with
// the module hierarchy.
//
// srcStripPrefix("/a/B/C", "B.C") == "/a"
//
// Actually, it doesn't check that the components of
// the module name match the path, so
//
// srcStripPrefix("/a/B/C", "D.E") == "/a"
//
func srcStripPrefix(file, modName string) string {
	numComponents := strings.Count(modName, ".") + 1
	dir := file
	for i := 0; i < numComponents; i++ {
		dir = filepath.Dir(dir)
	}
	return dir
}

func ruleNameFromRuleInfo(ruleInfo *RuleInfo) string {
	return ruleInfo.OriginatingRules[0].Name() + "." + ruleInfo.ModuleData.ModuleName
}

func shouldModularize(r *rule.Rule) bool {
	if r.ShouldKeep() {
		return false
	}
	for _, c := range r.Comments() {
		if strings.Trim(c, "# ") == "gazelle_haskell_modules:keep" {
			return false
		}
	}
	return true
}
