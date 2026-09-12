package emailmime

import (
	"fmt"
	"net/mail"

	"go.kenn.io/docbank/document"
)

func projectAddressesBounded(value string, limit int64) ([]document.EmailAddressV1, int64, error) {
	if limit < 0 || int64(len(value)) > Recipe().Limits.HeaderBytes {
		return nil, 0, errDisplayBudget
	}
	parser := mail.AddressParser{WordDecoder: &headerWordDecoder}
	parsed, err := parser.ParseList(value)
	if err != nil {
		return nil, 0, fmt.Errorf("parse email addresses: %w", err)
	}
	addresses := make([]document.EmailAddressV1, 0, len(parsed))
	var cost int64
	for _, address := range parsed {
		for _, text := range []string{address.Name, address.Address} {
			if _, err := boundedUTF8(text, limit-cost); err != nil {
				return nil, 0, err
			}
			cost += int64(len(text))
		}
		addresses = append(addresses, document.EmailAddressV1{Name: address.Name, Address: address.Address})
	}
	return addresses, cost, nil
}
