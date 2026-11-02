# Embedserver invariants

- Keep the pinned assets, tokenizer, and Ollama parity fixture in sync.
- The ONNX Runtime table indices require a runtime providing API 23 or newer, tested with 1.23.2.
- On shutdown, close HTTP connections, wait for inference, then close the model and remove its runtime record.
- Hold the kit-embed owner lock from before model load until runtime record removal.
- Decode requests as leniently as Ollama: replace invalid UTF-8 and lone surrogates with U+FFFD and keep the last duplicate member.
- Take the inference gate per input, so other requests run between batch items.
