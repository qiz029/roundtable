package roundtable

import (
	"database/sql"
	"fmt"
	"strings"
)

func twoPartAction(path string, prefix string) (string, string, bool) {
	parts := strings.Split(pathTail(path, prefix), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func isUniqueErr(err error) bool {
	return strings.Contains(fmt.Sprint(err), "UNIQUE constraint failed")
}
