package updater

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func newerVersion(current, candidate string) (bool, error) {
	c, err := parseVersion(current)
	if err != nil {
		return false, fmt.Errorf("current version: %w", err)
	}
	n, err := parseVersion(candidate)
	if err != nil {
		return false, fmt.Errorf("release version: %w", err)
	}
	for i := 0; i < 3; i++ {
		if n[i] != c[i] {
			return n[i] > c[i], nil
		}
	}
	return false, nil
}

func parseVersion(value string) ([3]int, error) {
	var result [3]int
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if index := strings.IndexByte(value, '-'); index >= 0 {
		value = value[:index]
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return result, errors.New("expected semantic version major.minor.patch")
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return result, errors.New("version contains a non-numeric component")
		}
		result[i] = n
	}
	return result, nil
}

func versionsEqual(a, b string) bool {
	av, errA := parseVersion(a)
	bv, errB := parseVersion(b)
	return errA == nil && errB == nil && av == bv
}
