package testrunner

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var condRe = regexp.MustCompile(`^(\S+)\s*(==|!=)\s*(.+)$`)

// evalCondition evaluates a `when:`/branch-case condition string against
// ctx, per 09 §6's grammar: `==`/`!=` with a literal-only RHS and
// stringified comparison (no numeric coercion), or bare truthiness.
func evalCondition(cond string, ctx *EvalContext) (bool, error) {
	cond = strings.TrimSpace(cond)
	if m := condRe.FindStringSubmatch(cond); m != nil {
		lhsPath, op, rhsLit := m[1], m[2], strings.TrimSpace(m[3])
		lhsVal, err := ctx.Resolve(lhsPath)
		if err != nil {
			return false, err
		}
		lhsStr := stringifyForCompare(lhsVal)
		rhsStr := stripQuotes(rhsLit)
		eq := lhsStr == rhsStr
		if op == "==" {
			return eq, nil
		}
		return !eq, nil
	}
	// bare truthiness
	v, err := ctx.Resolve(cond)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

func stringifyForCompare(v interface{}) string {
	switch tv := v.(type) {
	case nil:
		return ""
	case bool:
		if tv {
			return "true"
		}
		return "false"
	case string:
		return tv
	case float64:
		return strconv.FormatFloat(tv, 'g', -1, 64)
	case int:
		return strconv.Itoa(tv)
	case int64:
		return strconv.FormatInt(tv, 10)
	default:
		return fmt.Sprintf("%v", tv)
	}
}

func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func truthy(v interface{}) bool {
	switch tv := v.(type) {
	case nil:
		return false
	case bool:
		return tv
	case string:
		return tv != ""
	case float64:
		return tv != 0
	case int:
		return tv != 0
	case []interface{}:
		return len(tv) > 0
	case map[string]interface{}:
		return len(tv) > 0
	default:
		return true
	}
}
