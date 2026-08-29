package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxStdioRequestBytes = 1 << 20

var (
	errInvalidStdioFrame = errors.New("invalid MCP stdio frame")
	errStdioTransport    = errors.New("MCP stdio transport failed")
)

// ServeStdio runs one exact-version server over newline-delimited stdin and
// stdout. Diagnostics contain only stable error codes; request data never
// crosses onto the diagnostics stream.
func ServeStdio(
	ctx context.Context,
	server *Server,
	input io.ReadCloser,
	output io.Writer,
	logger *slog.Logger,
) error {
	if server == nil || input == nil || output == nil {
		return errStdioTransport
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	err := server.Run(ctx, &stdioTransport{input: input, output: output})
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return nil
	case errors.Is(err, errInvalidStdioFrame):
		logger.Error("MCP stdio stopped", "error_code", "invalid_frame")
		return errInvalidStdioFrame
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		logger.Error("MCP stdio stopped", "error_code", "transport_failure")
		return errStdioTransport
	}
}

type stdioTransport struct {
	input  io.ReadCloser
	output io.Writer
}

func (transport *stdioTransport) Connect(context.Context) (sdkmcp.Connection, error) {
	return &stdioConnection{
		input:  transport.input,
		reader: bufio.NewReaderSize(transport.input, maxStdioRequestBytes+2),
		output: transport.output,
	}, nil
}

func (*stdioTransport) SupportsProtocolVersion(version string) bool {
	return version == ProtocolVersion
}

type stdioConnection struct {
	input     io.ReadCloser
	reader    *bufio.Reader
	output    io.Writer
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func (connection *stdioConnection) Read(context.Context) (jsonrpc.Message, error) {
	frame, err := connection.reader.ReadSlice('\n')
	if errors.Is(err, io.EOF) && len(frame) == 0 {
		return nil, io.EOF
	}
	if err != nil || len(frame) > maxStdioRequestBytes+2 || len(frame) == 0 || frame[len(frame)-1] != '\n' {
		return nil, errInvalidStdioFrame
	}
	frame = frame[:len(frame)-1]
	if len(frame) > 0 && frame[len(frame)-1] == '\r' {
		frame = frame[:len(frame)-1]
	}
	if len(frame) == 0 || len(frame) > maxStdioRequestBytes || bytes.ContainsAny(frame, "\r\n") {
		return nil, errInvalidStdioFrame
	}
	message, decodeErr := jsonrpc.DecodeMessage(frame)
	if decodeErr != nil {
		return nil, errInvalidStdioFrame
	}
	return message, nil
}

func (connection *stdioConnection) Write(_ context.Context, message jsonrpc.Message) error {
	encoded, err := jsonrpc.EncodeMessage(message)
	if err != nil || bytes.ContainsAny(encoded, "\r\n") {
		return errStdioTransport
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	encoded = append(encoded, '\n')
	for len(encoded) > 0 {
		written, writeErr := connection.output.Write(encoded)
		if writeErr != nil {
			return errStdioTransport
		}
		if written <= 0 || written > len(encoded) {
			return errStdioTransport
		}
		encoded = encoded[written:]
	}
	return nil
}

func (connection *stdioConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.closeErr = connection.input.Close()
	})
	return connection.closeErr
}

func (*stdioConnection) SessionID() string { return "" }
