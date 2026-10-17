package docbank

import "errors"

type resetSourceAttributes struct {
	directory bool
	reparse   bool
}

func validateResetSourcePath(path string) error {
	attributes, err := resetSourceAttributesNoFollow(path)
	if err != nil {
		return err
	}
	return validateResetSourceAttributes(attributes)
}

func validateResetSourceAttributes(attributes resetSourceAttributes) error {
	if attributes.reparse {
		return errors.New("docbank reset source must not be a symlink or reparse point")
	}
	if !attributes.directory {
		return errors.New("docbank reset source must be one real existing directory")
	}
	return nil
}
