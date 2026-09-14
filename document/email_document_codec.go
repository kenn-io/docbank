package document

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// UnmarshalEmailDocumentJSON checks finite JSON byte, collection, nesting and
// integer budgets before allocating typed request or response collections.
func UnmarshalEmailDocumentJSON(raw []byte, out any) error {
	if len(raw) > EmailDocumentMaxJSONBytes {
		return errors.New("email document JSON exceeds byte limit")
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	type scope struct {
		kind  byte
		count int
	}
	stack := make([]scope, 0, 16)
	first := true
	for {
		token, err := decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		kind := byte(token.Kind())
		if first {
			first = false
			if kind != '{' {
				return errors.New("email document JSON must be an object")
			}
		}
		if kind == '}' || kind == ']' {
			if len(stack) == 0 {
				return errors.New("invalid email document JSON nesting")
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if len(stack) > 0 && stack[len(stack)-1].kind == '[' {
			stack[len(stack)-1].count++
			if stack[len(stack)-1].count > EmailDocumentMaxParts {
				return errors.New("email document JSON collection exceeds limit")
			}
		}
		if kind == '{' || kind == '[' {
			if len(stack) >= 32 {
				return errors.New("email document JSON nesting exceeds limit")
			}
			stack = append(stack, scope{kind: kind})
		}
		if kind == '0' {
			if strings.ContainsAny(token.String(), ".eE") {
				return errors.New("email document JSON requires integer numbers")
			}
			n, err := token.Int()
			if err != nil || n < -maxSafeJSONInteger || n > maxSafeJSONInteger {
				return errors.New("email document JSON integer exceeds safe range")
			}
		}
	}
	if first || len(stack) != 0 {
		return errors.New("incomplete email document JSON")
	}
	return json.Unmarshal(raw, out, json.RejectUnknownMembers(true))
}

// ValidateEmailDocumentReceipt validates portable shape. The catalog also
// checks every relation against retained MIME and ordinary version authority.
func ValidateEmailDocumentReceipt(r EmailDocumentPublicationReceipt) error {
	if err := ValidateEmailDocumentOperationID(r.OperationID); err != nil {
		return err
	}
	if !emailDocumentHash(r.RequestDigest) || len(r.Relations) > EmailDocumentMaxParts {
		return errors.New("invalid email document receipt")
	}
	if r.InventoryState != "complete" && r.InventoryState != "partial" {
		return errors.New("invalid email document inventory state")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.CreatedAt); err != nil {
		return fmt.Errorf("invalid email document receipt timestamp: %w", err)
	}
	for i, v := range r.Relations {
		if v.OperationID != r.OperationID || v.Order != i+1 || v.SiblingOrder < 0 || v.SiblingOrder > EmailDocumentMaxParts || !emailDocumentHash(v.GenerationID) || !emailDocumentHash(v.AttachmentID) || len(v.Filename) > 240 {
			return errors.New("invalid email document relation")
		}
		if err := ValidateEmailDocumentIdentity(v.Parent); err != nil {
			return err
		}
		if err := ValidateEmailPartPath(v.PartPath); err != nil {
			return err
		}
		switch v.Outcome {
		case "decoded":
			if v.Child == nil {
				return errors.New("decoded relation has no child")
			}
			if err := ValidateEmailDocumentIdentity(*v.Child); err != nil {
				return err
			}
		case "unsupported", "failed", "encrypted", "unavailable":
			if v.Child != nil {
				return errors.New("unavailable relation has a child")
			}
		default:
			return errors.New("invalid email document outcome")
		}
	}
	return nil
}
