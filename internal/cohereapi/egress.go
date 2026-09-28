package cohereapi

import (
	"errors"
	"fmt"
	"reflect"

	"go.kenn.io/docbank/document/providerhttp"
)

func NormalizeEgress(policy *providerhttp.EgressPolicy) error {
	if err := providerhttp.NormalizeHostedEgress(policy, "api.cohere.com"); err != nil {
		if errors.Is(err, providerhttp.ErrHostedAuthority) {
			return errors.New("cohere: egress authority must be exactly api.cohere.com:443")
		}
		return fmt.Errorf("cohere: %s", err.Error())
	}
	return nil
}

func IsNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
