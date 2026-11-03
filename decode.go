package enigmoss

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Structural checks of request values, the same as every official SDK makes. Only formats
// are checked here: the server stays the source of truth for business limits.
var (
	amountPattern   = regexp.MustCompile(`^\d{1,11}(\.\d{1,18})?$`)
	transferPattern = regexp.MustCompile(`^\d{1,11}(\.\d{1,8})?$`)
	orderIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	percentPattern  = regexp.MustCompile(`^\d+(\.\d{1,2})?$`)
	eventPattern    = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)
	numericPattern  = regexp.MustCompile(`^[ \t\n\r\v\f]*[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?[ \t\n\r\v\f]*$`)
)

func checkUUID(value, field string) (string, error) {
	if !uuidPattern.MatchString(value) {
		return "", fmt.Errorf("enigmoss: %s must be a UUID, got %q", field, value)
	}
	return strings.ToLower(value), nil
}

func checkOrderID(value, field string) error {
	if !orderIDPattern.MatchString(value) {
		return fmt.Errorf(`enigmoss: %s must be 1-128 letters, digits, "-" or "_"`, field)
	}
	return nil
}

func checkAmount(value, field string) error {
	if !amountPattern.MatchString(value) {
		return fmt.Errorf(`enigmoss: %s must be a positive decimal string such as "10.50", got %q`, field, value)
	}
	return nil
}

func checkLength(value, field string, minLen, maxLen int) error {
	// Characters, not bytes, like the server.
	if n := utf8.RuneCountInString(value); n < minLen || n > maxLen {
		return fmt.Errorf("enigmoss: %s must be %d-%d characters long", field, minLen, maxLen)
	}
	return nil
}

func checkRange(value int, field string, minValue, maxValue int) error {
	if value < minValue || value > maxValue {
		return fmt.Errorf("enigmoss: %s must be between %d and %d", field, minValue, maxValue)
	}
	return nil
}

func checkLimit(limit, maxValue int) error {
	if limit != 0 {
		return checkRange(limit, "Limit", 1, maxValue)
	}
	return nil
}

func checkEvent(value, field string) error {
	if value != "" && !eventPattern.MatchString(value) {
		return fmt.Errorf(`enigmoss: %s must look like "payment.paid"`, field)
	}
	return nil
}

// firstError returns the first non-nil error.
func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// queryTime formats a filter time in UTC with a literal "Z" and no fractional seconds
// (the events API rejects fractions). The zero time is no filter.
func queryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

func queryInt(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// decimalFields are the money and rate fields of the responses. They are JSON strings; a
// value that is not numeric is rejected, so a broken amount never reaches money logic.
var decimalFields = map[string]bool{
	"amount": true, "amount_usd": true, "balance": true, "balance_usd": true, "exchange_rate": true,
	"fee": true, "max_amount": true, "min_amount": true, "percent": true, "rate": true, "source_amount": true,
}

// decode reads the "data" of a response into out and checks it: decimal fields are
// numeric and times are UTC. An unreadable 2xx is an *Error whose outcome is unknown.
func decode(resp *response, out any) error {
	if err := decodeJSON(resp.data, out); err != nil {
		return unreadable(resp.status, resp.requestID, "The response does not match the API: "+err.Error()+".")
	}
	return nil
}

func decodeJSON(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(out); err != nil {
		return err
	}
	return normalize(reflect.ValueOf(out), "data")
}

var timeType = reflect.TypeOf(time.Time{})

func normalize(v reflect.Value, path string) error {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return normalize(v.Elem(), path)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 { // json.RawMessage
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := normalize(v.Index(i), path+"."+strconv.Itoa(i)); err != nil {
				return err
			}
		}
	case reflect.Struct:
		if v.Type() == timeType {
			if v.CanSet() {
				v.Set(reflect.ValueOf(v.Interface().(time.Time).UTC()))
			}
			return nil
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			fv := v.Field(i)
			if fv.Kind() == reflect.String && decimalFields[name] && fv.String() != "" && !numericPattern.MatchString(fv.String()) {
				return fmt.Errorf("%s.%s is not a decimal string: %q", path, name, fv.String())
			}
			if err := normalize(fv, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

// one executes a call that returns one object.
func one[T any](ctx context.Context, c *core, cl call) (*T, error) {
	resp, err := c.do(ctx, cl)
	if err != nil {
		return nil, err
	}
	out := new(T)
	if err := decode(resp, out); err != nil {
		return nil, err
	}
	return out, nil
}

// many executes a call that returns a list without pagination.
func many[T any](ctx context.Context, c *core, cl call) ([]*T, error) {
	resp, err := c.do(ctx, cl)
	if err != nil {
		return nil, err
	}
	var out []*T
	if err := decode(resp, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// page executes a call that returns one page of a list.
func page[T any](ctx context.Context, c *core, cl call) (*Page[T], error) {
	resp, err := c.do(ctx, cl)
	if err != nil {
		return nil, err
	}
	return decodePage[T](resp)
}
