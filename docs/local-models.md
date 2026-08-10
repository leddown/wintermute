# Running open-weight models on Linux Mint with a GTX 1070

This is the setup guide for the machine that serves models to wintermute. It targets a
Linux Mint host with a single GTX 1070 and produces an OpenAI-compatible API on the LAN
that `wintermuted` — and anything else on your network — can talk to.

If you just want something working and will tune later, the
[quickstart](quickstart.md) gets there in one command with Ollama. This guide is the
version you keep.

Read [Know your hardware](#0-know-your-hardware) first. The GTX 1070 is a capable card for
this, but it is a *Pascal* card, and Pascal has two properties that invalidate most generic
"run local LLMs" tutorials. Getting those two things wrong is the difference between
40 tokens/sec and a card that sits idle while your CPU does the work.

---

## 0. Know your hardware

| | |
|---|---|
| GPU | GeForce GTX 1070 (GP104, Pascal) |
| Compute capability | **6.1** |
| VRAM | **8 GB** GDDR5 |
| Memory bandwidth | **256 GB/s** |
| Tensor cores | **None** |
| FP16 throughput | **1/64 of FP32** — effectively unusable |
| INT8 | Fast, via the `__dp4a` 4-way dot-product instruction |

Two consequences drive every decision below.

**Half precision is a trap on this card.** GP104 runs FP16 at 1/64 the rate of FP32. Any
guide that tells you to run an unquantized FP16 model is describing a different GPU. What
Pascal *does* have is a fast 8-bit integer dot product (`__dp4a`), which is exactly what
llama.cpp's MMQ quantized-matmul kernels are built on. So 4-bit and 5-bit k-quants are not
a quality compromise you accept to save memory — on this card they are also the *fast*
path. Q4_K_M is the sweet spot in both dimensions at once.

**Pascal is in its final driver year.** NVIDIA's **580** branch is the last Linux driver
line that supports Maxwell, Pascal and Volta; **590 drops the GTX 10-series entirely**.
Separately, **CUDA 13 removed offline compilation for Pascal**, so anything you build from
source must be built against **CUDA 12.x**. Nothing here is broken — the 580 branch
continues to get kernel compatibility and critical fixes — but you must pin your versions
rather than blindly taking the newest of everything.

### What actually fits in 8 GB

Rough VRAM at 4-bit (Q4_K_M), model weights only, before context:

| Model size | Weights @ Q4_K_M | Verdict on 8 GB |
|---|---|---|
| 3–4B | ~2.2–2.6 GB | Comfortable, long context available |
| 7–8B | ~4.4–4.9 GB | **The sweet spot** — full GPU offload with 8–16K context |
| 12–14B | ~7.3–8.5 GB | Tight to over — needs short context, or partial CPU offload |
| 24B+ | ~14 GB+ | Partial offload only; expect single-digit tokens/sec |
| 30B MoE (A3B) | ~17 GB | Doesn't fit, but only ~3B active — usable with CPU offload |

Add context on top of weights. A 8K-token KV cache costs roughly 0.5–1 GB at fp16 for a
modern GQA model, and half that if you quantize the cache (see step 4).

---

## 1. Install the NVIDIA 580 driver

Mint's Driver Manager is the safe route.

```bash
# See what's available and what's currently in use.
ubuntu-drivers devices
nvidia-smi
```

Open **Driver Manager** from the menu and select the **580** series driver. If you install
from the terminal instead:

```bash
sudo apt update
sudo apt install nvidia-driver-580
sudo reboot
```

> **Do not install 590 or newer.** It does not support the GTX 10-series. If your package
> manager offers a newer branch, pin 580:
> ```bash
> sudo apt-mark hold nvidia-driver-580
> ```

Verify after reboot:

```bash
nvidia-smi
```

You want a driver version starting `580.`, `GeForce GTX 1070` listed, and `8192MiB` total
memory. If `nvidia-smi` reports "No devices were found", stop here and fix the driver —
nothing downstream will work.

---

## 2. Pick a serving path

You need one process that loads a model and exposes an HTTP API. Two good options; they are
not mutually exclusive, and wintermute talks to both.

| | **llama.cpp** (`llama-server`) | **Ollama** |
|---|---|---|
| Setup | Build from source (~15 min) | One command |
| Pascal tuning | Full control: quant type, KV cache type, offload split | Mostly automatic |
| Model management | You download GGUFs yourself | `ollama pull`, built-in registry |
| API | OpenAI **and** Anthropic compatible, plus `/props`, `/slots`, `/metrics` | Ollama API + OpenAI-compatible layer |
| Concurrency | Continuous batching, configurable slots | Sequential per model |
| Best for | Squeezing the most out of 8 GB | Getting running in five minutes |

**Recommendation:** start with Ollama to confirm the machine works end to end, then build
llama.cpp for the setup you actually keep. Step 6 puts llama-swap in front so you can serve
several models through one address regardless of which you chose.

---

## 3. Path A — Ollama (quick start)

Ollama bundles its own CUDA runtime and supports compute capability 5.0 and up, so the
1070 (6.1) works with no toolkit installation.

```bash
curl -fsSL https://ollama.com/install.sh | sh
```

Pull a model sized for the card:

```bash
ollama pull qwen3:8b          # strong general model, reliable tool calling
ollama pull gemma3:4b         # small, fast, multimodal
```

Confirm it is on the **GPU**, not the CPU:

```bash
ollama run qwen3:8b "hello"
ollama ps
```

The `PROCESSOR` column must say `100% GPU`. If it says `CPU` or splits, the model is too
large for free VRAM — drop to a smaller model or a tighter quant.

### Expose it on the LAN

By default Ollama listens on localhost only. Override it with a systemd drop-in:

```bash
sudo systemctl edit ollama
```

```ini
[Service]
Environment="OLLAMA_HOST=0.0.0.0:11434"
Environment="OLLAMA_KEEP_ALIVE=30m"
Environment="OLLAMA_FLASH_ATTENTION=1"
Environment="OLLAMA_KV_CACHE_TYPE=q8_0"
```

```bash
sudo systemctl restart ollama
curl http://localhost:11434/v1/models
```

`OLLAMA_KEEP_ALIVE` stops it unloading the model between wintermute turns — without it you
pay several seconds of reload on every request. See step 7 before opening the port to your
network.

---

## 4. Path B — llama.cpp built for Pascal

This is where the card's real performance is.

### Install the toolchain and CUDA 12.x

```bash
sudo apt install build-essential cmake git libcurl4-openssl-dev
```

Install the **CUDA 12.x** toolkit. CUDA 13 cannot generate code for Pascal at all — if
`nvcc --version` reports 13.x, you will get a build that ignores your GPU.

```bash
# Check what you have; you want release 12.x
nvcc --version
```

If CUDA 12.x is not installed, get it from NVIDIA's archive (choose the *deb (network)*
installer for Ubuntu 22.04, which Mint 21.x is based on — match your Mint base with
`cat /etc/upstream-release/lsb-release`) and install only the toolkit, **not** the bundled
driver:

```bash
sudo apt install cuda-toolkit-12-6
echo 'export PATH=/usr/local/cuda-12.6/bin:$PATH' >> ~/.bashrc
source ~/.bashrc
nvcc --version
```

### Build

The `CMAKE_CUDA_ARCHITECTURES=61` flag is the important one — it targets Pascal's compute
capability 6.1 specifically. Without it the build may target only newer architectures.

```bash
git clone https://github.com/ggml-org/llama.cpp
cd llama.cpp

cmake -B build \
  -DGGML_CUDA=ON \
  -DCMAKE_CUDA_ARCHITECTURES=61 \
  -DGGML_CUDA_FA_ALL_QUANTS=ON \
  -DCMAKE_BUILD_TYPE=Release

cmake --build build --config Release -j$(nproc)
```

`GGML_CUDA_FA_ALL_QUANTS=ON` compiles the full set of flash-attention kernel variants. It
makes the build noticeably slower, and it matters: without it, using *mismatched* K and V
cache quantization types (say `q8_0` for K and `q4_0` for V) silently falls back to a CPU
attention path, which will look like a mysterious 10× slowdown. With this flag on you can
mix freely.

The binaries land in `build/bin/`. Add them to your path:

```bash
echo 'export PATH=$HOME/llama.cpp/build/bin:$PATH' >> ~/.bashrc
source ~/.bashrc
```

### Get a model

Download a GGUF directly from Hugging Face. `bartowski` and `unsloth` are the reliable
quantizers.

```bash
mkdir -p ~/models && cd ~/models

# Qwen3 8B, Q4_K_M — the default recommendation for this card.
curl -L -O https://huggingface.co/bartowski/Qwen_Qwen3-8B-GGUF/resolve/main/Qwen_Qwen3-8B-Q4_K_M.gguf
```

### Run it

```bash
llama-server \
  --model ~/models/Qwen_Qwen3-8B-Q4_K_M.gguf \
  --alias qwen3-8b \
  --n-gpu-layers 99 \
  --ctx-size 16384 \
  --flash-attn on \
  --cache-type-k q8_0 \
  --cache-type-v q8_0 \
  --jinja \
  --parallel 2 \
  --host 0.0.0.0 \
  --port 8080 \
  --api-key "$(cat ~/.llama-api-key)"
```

What each of the non-obvious flags buys you:

- **`--n-gpu-layers 99`** — offload every layer to the GPU. Any number ≥ the model's layer
  count means "all". If the model doesn't fit, lower this until it does; each layer left on
  the CPU costs you real throughput.
- **`--ctx-size 16384`** — context window. This is a *VRAM* decision, not a preference:
  the KV cache grows linearly with it. Start here and reduce if you're tight.
- **`--flash-attn on`** — cuts KV cache memory and speeds up attention.
- **`--cache-type-k q8_0 --cache-type-v q8_0`** — quantize the KV cache to 8-bit. This
  roughly halves the memory cost of context for a quality loss that is negligible in
  practice. On an 8 GB card this is most of what makes a long context affordable.
- **`--jinja`** — use the model's own chat template from the GGUF metadata. **Required for
  tool calling to work.** Without it, llama.cpp uses a generic template, the model never
  sees the tool-call format it was trained on, and wintermute's tool calls will fail in
  confusing ways — the model writes what looks like a tool call into its visible text
  instead of emitting a real one.
- **`--parallel 2`** — two concurrent request slots, so a second service on your network
  isn't blocked behind a long wintermute turn. Note that the context size is divided among
  slots.
- **`--api-key`** — see step 7. Generate one with `openssl rand -hex 32 > ~/.llama-api-key`.

### Verify

```bash
curl http://localhost:8080/health
curl http://localhost:8080/v1/models -H "Authorization: Bearer $(cat ~/.llama-api-key)"

curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $(cat ~/.llama-api-key)" \
  -d '{"model":"qwen3-8b","messages":[{"role":"user","content":"Say hi in five words."}]}'
```

Watch `nvidia-smi` while that runs. You should see memory allocated and the GPU busy. If
VRAM usage is near zero, your build didn't pick up CUDA — rebuild and check that the cmake
output says `GGML_CUDA` is enabled.

### Benchmark it

```bash
llama-bench -m ~/models/Qwen_Qwen3-8B-Q4_K_M.gguf -ngl 99
```

On a healthy GTX 1070 with an 8B model at Q4_K_M, expect roughly **30–45 tokens/sec** for
generation. Substantially below that usually means one of: layers spilling to CPU, thermal
throttling (this card commonly loses 20–40% under sustained load if its cooling is tired —
check with `nvidia-smi -q -d TEMPERATURE`), or a non-CUDA build.

---

## 5. Choosing a model

Sized for 8 GB. Tool calling is called out because wintermute is an agent — a model that
can't reliably emit tool calls will be frustrating in this application regardless of how
well it writes.

| Model | Quant | ~VRAM | Tool calling | Use it for |
|---|---|---|---|---|
| **Qwen3 8B** | Q4_K_M | ~4.9 GB | Strong | **Default.** General work, agent/tool use, the wintermute rename flow |
| Qwen3 4B | Q5_K_M | ~2.9 GB | Strong | When you need headroom for long context |
| Gemma 3 4B | Q4_K_M | ~2.6 GB | Good | Fast responses, multimodal (images) |
| Gemma 3 12B QAT | Q4_K_M | ~7.3 GB | Good | Best writing quality that still fits; short context only |
| Llama 3.1 8B | Q4_K_M | ~4.7 GB | Good | Well-understood baseline, huge ecosystem |
| Phi-4 mini | Q4_K_M | ~2.5 GB | Fair | Reasoning on a tight budget |
| Granite 4 | Q4_K_M | varies | Strong | Explicitly trained for function calling |
| Qwen3-Coder 30B A3B | Q4_K_M | ~17 GB | Strong | Code. Doesn't fit — but it's a MoE with ~3B active, so CPU offload stays usable |

**For document generation specifically** — long, coherent prose rather than short answers —
the ranking changes. Prioritize parameter count over speed, because writing quality tracks
model size closely and you are generating in the background rather than waiting on a chat
response. On this card that means **Gemma 3 12B QAT at Q4_K_M with a reduced context**
(the QAT — quantization-aware trained — release holds up notably better at 4-bit than a
naive post-training quant of the same model), with **Qwen3 8B** as the faster fallback when
you need the context window for source material. Don't take the table above as final —
`POST /api/v1/models/plan` runs this ranking against your *actual measured* free VRAM
(step 8), and the assistant can call the same thing itself via `recommend_model`.

Quantization guide for this card:

- **Q4_K_M** — default. Best quality-per-byte, and hits Pascal's fast INT8 path.
- **Q5_K_M** — use when the model is small enough that you have room to spare.
- **Q6_K / Q8_0** — only for models under ~4B. Rarely worth it over a bigger model at Q4.
- **IQ4_XS** — slightly smaller than Q4_K_M; useful to squeeze a 14B in, but the
  i-quant kernels are less optimized on older cards. Benchmark before committing.
- **F16** — never, on this GPU. See step 0.

---

## 6. Serve several models through one address (llama-swap)

`llama-server` holds one model. With 8 GB you can only have one loaded anyway — but you'll
want to switch between them without SSHing in. [llama-swap](https://github.com/mostlygeek/llama-swap)
is a single Go binary that sits in front, reads the `model` field of each request, and
starts or swaps the right upstream automatically.

```bash
# Download the latest release binary for linux-amd64 from
# https://github.com/mostlygeek/llama-swap/releases
mkdir -p ~/.config/llama-swap
```

`~/.config/llama-swap/config.yaml`:

```yaml
healthCheckTimeout: 300

models:
  qwen3-8b:
    # Unload after 10 minutes idle so the VRAM comes back.
    ttl: 600
    cmd: >
      /home/YOU/llama.cpp/build/bin/llama-server
      --model /home/YOU/models/Qwen_Qwen3-8B-Q4_K_M.gguf
      --port ${PORT}
      --n-gpu-layers 99 --ctx-size 16384
      --flash-attn on --cache-type-k q8_0 --cache-type-v q8_0
      --jinja

  gemma3-12b:
    ttl: 600
    cmd: >
      /home/YOU/llama.cpp/build/bin/llama-server
      --model /home/YOU/models/gemma-3-12b-it-qat-Q4_K_M.gguf
      --port ${PORT}
      --n-gpu-layers 99 --ctx-size 4096
      --flash-attn on --cache-type-k q8_0 --cache-type-v q8_0
      --jinja

  gemma3-4b:
    ttl: 600
    cmd: >
      /home/YOU/llama.cpp/build/bin/llama-server
      --model /home/YOU/models/gemma-3-4b-it-Q4_K_M.gguf
      --port ${PORT}
      --n-gpu-layers 99 --ctx-size 32768
      --flash-attn on --cache-type-k q8_0 --cache-type-v q8_0
      --jinja
```

Run it:

```bash
llama-swap --config ~/.config/llama-swap/config.yaml --listen 0.0.0.0:8080
```

Now `GET /v1/models` lists all three, and a request naming `gemma3-12b` swaps to it
transparently. Note the differing `--ctx-size` per entry — that's the 8 GB budget being
spent differently depending on model size, exactly the trade-off `POST /api/v1/models/fit`
estimates for you.

> **Swapping is not concurrency.** Only one of these is resident at a time. If two
> wintermute conversations use different models through one llama-swap, every alternating
> request unloads and reloads gigabytes of weights, and you can spend more wall-clock time
> loading than generating. See [backends.md](backends.md#gotchas-that-will-cost-you-performance).

---

## 7. Network exposure and security

You are putting an unauthenticated-by-default text generator on your LAN. Two rules.

**Require an API key.** `llama-server --api-key`, or put llama-swap behind a reverse proxy
that enforces one. Ollama has no built-in authentication at all — if you expose Ollama
beyond localhost, put a proxy in front of it.

```bash
openssl rand -hex 32 > ~/.llama-api-key
chmod 600 ~/.llama-api-key
```

**Restrict it to your subnet.** Do not port-forward this to the internet.

```bash
sudo ufw allow from 192.168.1.0/24 to any port 8080 proto tcp
sudo ufw enable
sudo ufw status
```

Adjust the subnet to match your network (`ip -4 addr show` to find it).

### Run it as a service

`/etc/systemd/system/llama-swap.service`:

```ini
[Unit]
Description=llama-swap OpenAI-compatible LLM gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=YOU
Environment="PATH=/usr/local/cuda-12.6/bin:/usr/local/bin:/usr/bin:/bin"
ExecStart=/usr/local/bin/llama-swap --config /home/YOU/.config/llama-swap/config.yaml --listen 0.0.0.0:8080
Restart=on-failure
RestartSec=5

# The service only needs to read models and talk to the GPU.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/YOU/.cache

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now llama-swap
sudo systemctl status llama-swap
journalctl -u llama-swap -f
```

---

## 8. Point wintermute at it

`wintermuted` reads its backends from `backends.json` (override the path with
`WINTERMUTE_BACKENDS`). A minimal configuration for the setup above:

```json
{
  "default": "local",
  "backends": [
    {
      "name": "local",
      "kind": "llamacpp",
      "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY",
      "model": "qwen3-8b"
    }
  ]
}
```

Or skip the file entirely for a single backend:

```bash
export WINTERMUTE_LLM_PROVIDER=openai
export WINTERMUTE_LLM_BASE_URL=http://127.0.0.1:8080/v1
export WINTERMUTE_LLM_MODEL=qwen3-8b
export WINTERMUTE_LLM_API_KEY=$(cat ~/.llama-api-key)
```

To keep Claude available as an alternative to switch to per session, add a second backend:

```json
{
  "default": "local",
  "backends": [
    { "name": "local",  "kind": "llamacpp",  "base_url": "http://127.0.0.1:8080/v1",
      "api_key_env": "LLAMA_API_KEY", "model": "qwen3-8b" },
    { "name": "claude", "kind": "anthropic", "api_key_env": "ANTHROPIC_API_KEY",
      "model": "claude-opus-5" }
  ]
}
```

Then:

```bash
go run ./cmd/wintermuted
```

Verify what the server sees. `$TOKEN` is a client token from
`wintermuted -add-client <name>`:

```bash
# Hardware: should report the GTX 1070, driver version, total and free VRAM.
curl -s localhost:8080/api/v1/system -H "Authorization: Bearer $TOKEN" | jq

# Backend health: status must be "ok".
curl -s localhost:8080/api/v1/backends -H "Authorization: Bearer $TOKEN" | jq

# What each backend serves, with a fit verdict against actual free VRAM.
curl -s "localhost:8080/api/v1/models?context=8192" -H "Authorization: Bearer $TOKEN" | jq
```

If a backend reports `unreachable`, the URL or API key is wrong — `journalctl -u
llama-swap` will usually say why. Re-probe without restarting the server with
`POST /api/v1/backends/refresh`.

For more than one backend — a second machine, a small model beside a large one, Claude
alongside local — see [backends.md](backends.md).

---

## 9. Troubleshooting

**Model runs on CPU despite `-ngl 99`.** The model plus its KV cache exceeds free VRAM.
Check `nvidia-smi` for what else is using the card — a desktop session can hold 300–600 MB.
Reduce `--ctx-size` first, then step down a quant, then step down a model size.

**Tool calls never fire, or the model describes calling a tool instead of calling it.** You
are missing `--jinja`. The model isn't seeing its trained tool-call format.

**Sudden 10× slowdown with quantized KV cache.** Mismatched `--cache-type-k` and
`--cache-type-v` on a build without `GGML_CUDA_FA_ALL_QUANTS=ON` falls back to CPU
attention. Either rebuild with the flag or set both cache types to the same value.

**Throughput degrades over a long session.** Thermal throttling. `nvidia-smi -q -d
TEMPERATURE,PERFORMANCE`. A 2016-vintage cooler on a card under sustained load is a
genuinely common cause of 20–40% loss.

**`nvidia-smi` works but the build ignores the GPU.** Almost always CUDA 13 — it cannot
generate Pascal code. `nvcc --version` must report 12.x. Rebuild after fixing.

**An `apt upgrade` broke the driver.** You were moved to 590+. Reinstall `nvidia-driver-580`
and `apt-mark hold` it.

---

## References

- [llama.cpp server documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)
- [llama-swap](https://github.com/mostlygeek/llama-swap)
- [Ollama GPU documentation](https://ollama.readthedocs.io/en/gpu/)
- [NVIDIA 580 legacy branch announcement](https://www.techpowerup.com/338497/nvidias-v580-driver-branch-ends-support-for-maxwell-pascal-and-volta-gpus)
- [CUDA drops Maxwell/Pascal/Volta offline compilation](https://www.tomshardware.com/pc-components/gpus/nvidia-to-drop-cuda-support-for-maxwell-pascal-and-volta-gpus-with-the-next-major-toolkit-release)
- [NVIDIA Pascal tuning guide (FP16 and INT8 rates)](https://docs.nvidia.com/cuda/pascal-tuning-guide/index.html)

### In this repository

- [Quickstart](quickstart.md) — the shortest path to a working setup
- [Running several backends](backends.md) — more than one model source, and what that
  does and doesn't make faster
- [README](../README.md) — configuration reference and the server API