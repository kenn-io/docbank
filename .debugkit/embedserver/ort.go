package embedserver

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

// OrtApi indices require a runtime providing API 23 or newer, tested with 1.23.2.
type ort struct {
	createOptions, getAllocator func(*uintptr) uintptr

	library, env, session, allocator uintptr
	errorMessage                     func(uintptr) string
	createEnv                        func(int32, string, *uintptr) uintptr
	createSession                    func(uintptr, unsafe.Pointer, uintptr, *uintptr) uintptr
	run                              func(uintptr, uintptr, []*byte, []uintptr, uintptr, []*byte, uintptr, *uintptr) uintptr
	setOptimization                  func(uintptr, int32) uintptr
	createTensor                     func(uintptr, []int64, uintptr, int32, *uintptr) uintptr
	tensorData                       func(uintptr, *unsafe.Pointer) uintptr

	releaseEnv, releaseStatus, releaseSession, releaseValue, releaseOptions func(uintptr)
}

func openORT(library, model string) (_ *ort, err error) {
	o := new(ort)
	o.library, err = openLibrary(library)
	if err != nil {
		return nil, fmt.Errorf("load ONNX Runtime: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, o.Close())
		}
	}()
	symbol, err := librarySymbol(o.library, "OrtGetApiBase")
	if err != nil {
		return nil, err
	}
	var base func() unsafe.Pointer
	purego.RegisterFunc(&base, symbol)
	b := (*uintptr)(base())
	var getAPI func(uint32) unsafe.Pointer
	purego.RegisterFunc(&getAPI, *b)
	table := getAPI(23)
	if table == nil {
		return nil, errors.New("ONNX Runtime API 23 unavailable")
	}
	api := unsafe.Slice((*uintptr)(table), 101)
	purego.RegisterFunc(&o.errorMessage, api[2])
	purego.RegisterFunc(&o.createEnv, api[3])
	purego.RegisterFunc(&o.createSession, api[7])
	purego.RegisterFunc(&o.run, api[9])
	purego.RegisterFunc(&o.createOptions, api[10])
	purego.RegisterFunc(&o.setOptimization, api[23])
	purego.RegisterFunc(&o.createTensor, api[48])
	purego.RegisterFunc(&o.tensorData, api[51])
	purego.RegisterFunc(&o.getAllocator, api[78])
	purego.RegisterFunc(&o.releaseEnv, api[92])
	purego.RegisterFunc(&o.releaseStatus, api[93])
	purego.RegisterFunc(&o.releaseSession, api[95])
	purego.RegisterFunc(&o.releaseValue, api[96])
	purego.RegisterFunc(&o.releaseOptions, api[100])
	if err := o.check(o.createEnv(3, "kit-embed", &o.env)); err != nil {
		return nil, err
	}
	var options uintptr
	if err := o.check(o.createOptions(&options)); err != nil {
		return nil, err
	}
	defer o.releaseOptions(options)
	// Extended layer-normalization fusion in 1.23.2 cannot load this FP16
	// graph. Basic optimizations preserve its cast nodes and numeric behavior.
	if err := o.check(o.setOptimization(options, 1)); err != nil {
		return nil, err
	}
	path, err := modelPath(model)
	if err != nil {
		return nil, err
	}
	var pin runtime.Pinner
	pin.Pin(path)
	defer pin.Unpin()
	if err := o.check(o.createSession(o.env, path, options, &o.session)); err != nil {
		return nil, err
	}
	if err := o.check(o.getAllocator(&o.allocator)); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *ort) check(status uintptr) error {
	if status == 0 {
		return nil
	}
	message := o.errorMessage(status)
	o.releaseStatus(status)
	return fmt.Errorf("ONNX Runtime: %s", message)
}

func (o *ort) Close() error {
	if o.session != 0 {
		o.releaseSession(o.session)
	}
	if o.env != 0 {
		o.releaseEnv(o.env)
	}
	return closeLibrary(o.library)
}

func (o *ort) tensor(values []int) (uintptr, error) {
	var value uintptr
	if err := o.check(o.createTensor(o.allocator, []int64{1, int64(len(values))}, 2, 7, &value)); err != nil {
		return 0, err
	}
	var data unsafe.Pointer
	if err := o.check(o.tensorData(value, &data)); err != nil {
		o.releaseValue(value)
		return 0, err
	}
	tensor := unsafe.Slice((*int64)(data), len(values))
	for i := range values {
		tensor[i] = int64(values[i])
	}
	return value, nil
}

func (o *ort) infer(ctx context.Context, ids []int) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := make([]uintptr, 0, 3)
	defer func() {
		for _, v := range values {
			o.releaseValue(v)
		}
	}()
	mask := make([]int, len(ids))
	for i := range mask {
		mask[i] = 1
	}
	for _, input := range [][]int{ids, mask, make([]int, len(ids))} {
		value, err := o.tensor(input)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	names := [][]byte{[]byte("input_ids\x00"), []byte("attention_mask\x00"), []byte("token_type_ids\x00"), []byte("last_hidden_state\x00")}
	var pin runtime.Pinner
	defer pin.Unpin()
	pointers := make([]*byte, len(names))
	for i := range names {
		pin.Pin(&names[i][0])
		pointers[i] = &names[i][0]
	}
	pin.Pin(&pointers[0])
	var output uintptr
	status := o.run(o.session, 0, pointers[:3], values, 3, pointers[3:], 1, &output)
	defer o.releaseValue(output)
	if err := o.check(status); err != nil {
		return nil, err
	}
	var data unsafe.Pointer
	if err := o.check(o.tensorData(output, &data)); err != nil {
		return nil, err
	}
	hidden := unsafe.Slice((*float32)(data), len(ids)*768)
	pooled := make([]float32, 768)
	for i := range ids {
		for j := range pooled {
			pooled[j] += hidden[i*768+j] / float32(len(ids))
		}
	}
	return pooled, nil
}
