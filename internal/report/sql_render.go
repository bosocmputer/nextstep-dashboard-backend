package report

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var placeholderPattern = regexp.MustCompile(`\$[0-9]+`)

type Query struct {
	SQL  string
	Args []any
}

func RenderSQL(query Query) (string, error) {
	if strings.TrimSpace(query.SQL) == "" {
		return "", errors.New("SQL query is empty")
	}
	var renderErr error
	rendered := placeholderPattern.ReplaceAllStringFunc(query.SQL, func(placeholder string) string {
		if renderErr != nil {
			return placeholder
		}
		index, err := strconv.Atoi(strings.TrimPrefix(placeholder, "$"))
		if err != nil || index < 1 || index > len(query.Args) {
			renderErr = errors.New("SQL placeholder has no matching value")
			return placeholder
		}
		literal, err := postgresLiteral(query.Args[index-1])
		if err != nil {
			renderErr = err
			return placeholder
		}
		return literal
	})
	if renderErr != nil {
		return "", renderErr
	}
	return rendered, nil
}

func postgresLiteral(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "null", nil
	case string:
		return quotePostgresString(typed), nil
	case bool:
		if typed {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.Itoa(typed), nil
	case int32:
		return strconv.FormatInt(int64(typed), 10), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case uint:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(typed), 10), nil
	case uint64:
		return strconv.FormatUint(typed, 10), nil
	case float32:
		if math.IsNaN(float64(typed)) || math.IsInf(float64(typed), 0) {
			return "", errors.New("SQL number parameter must be finite")
		}
		return strconv.FormatFloat(float64(typed), 'g', -1, 32), nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return "", errors.New("SQL number parameter must be finite")
		}
		return strconv.FormatFloat(typed, 'g', -1, 64), nil
	case time.Time:
		return quotePostgresString(typed.UTC().Format(time.RFC3339Nano)), nil
	case []string:
		if len(typed) == 0 {
			return "array[]::text[]", nil
		}
		items := make([]string, len(typed))
		for index, item := range typed {
			items[index] = quotePostgresString(item)
		}
		return "array[" + strings.Join(items, ", ") + "]::text[]", nil
	default:
		return "", fmt.Errorf("unsupported SQL parameter type %T", value)
	}
}

func quotePostgresString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
