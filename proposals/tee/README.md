# Confidential TEE Inference (Minimal Production Flow)

This implementation keeps ordinary Gonka inference economics and chain lifecycle, but adds encrypted payload relay through dAPI to TEE executors.

## What is production-ready in this branch

- Same chain inference lifecycle as ordinary flow:
  - `MsgStartInference` on transfer side
  - `MsgFinishInference` on executor side
- Same payment counters and settlement path as ordinary flow.
- Encrypted request/response payloads (`AES-GCM`) are never decrypted by transfer dAPI.
- Executor is selectable and pinnable (`executor_id`) instead of random-only routing.
- TEE inferences are marked as confidential and skipped from external provider validation pipeline.
- Chain-level `MsgValidation` for confidential inference is explicitly rejected.
- Backward-compatible: ordinary `/v1/chat/completions` flow is untouched.

## Components

- `decentralized-api/internal/server/public/tee_handlers.go`
  - `POST /v1/tee/sessions/reserve`
  - `POST /v1/tee/chat/completions` (transfer relay)
  - `POST /v1/tee/executor/chat/completions` (executor relay + finish tx)
  - `POST /v1/tee/sessions/close`
- `decentralized-api/internal/teecrypto/teecrypto.go`
  - X25519 + HKDF + AES-GCM
- `decentralized-api/cmd/tee-node/main.go`
  - TEE adapter (decrypt -> vLLM/OpenAI endpoint -> encrypt)
- `decentralized-api/cmd/openai-tee-connector/main.go`
  - CLI connector for reserve/encrypt/infer/decrypt flow

## Chain provider model

Uses existing participant + hardware entities:

- `Participant.inference_url`: executor dAPI base URL
- `Participant.worker_public_key`: TEE encryption public key (X25519, base64)
- `HardwareNode` with:
  - model binding
  - `hardware.type` including `TEE`/`TDX`/`SEV-SNP`/`CONFIDENTIAL`
  - `host`/`port` as TEE adapter endpoint used by executor dAPI

## End-to-end flow

```text
Client
  -> POST /v1/tee/sessions/reserve (transfer dAPI)
       select TEE executor from chain
       create TEE session (routing + key metadata)

  -> POST /v1/tee/chat/completions (transfer dAPI)
       StartInference tx (same chain lifecycle)
       forward encrypted body to executor dAPI

  -> POST /v1/tee/executor/chat/completions (executor dAPI)
       relay ciphertext to local tee-node adapter
       receive encrypted response
       FinishInference tx (same chain lifecycle)
       return encrypted response

Client decrypts response locally.
```

## Run in production-like mode

### 1) Start tee-node adapter on executor host

Dry-run mode:

```bash
cd decentralized-api
go run ./cmd/tee-node --listen :18180 --dry-run
```

Real vLLM upstream:

```bash
cd decentralized-api
go run ./cmd/tee-node \
  --listen :18180 \
  --upstream-url http://127.0.0.1:8000/v1/chat/completions
```

`tee-node` prints `node_public_key` at startup.

### 2) Register executor in chain

1. Submit `MsgSubmitNewParticipant`:
   - `inference_url = https://<executor-dapi-host>`
   - `worker_key = <tee-node node_public_key>`
2. Submit `MsgSubmitHardwareDiff` with TEE-marked node:
   - `local_id = tee-1`
   - `models = [Qwen/Qwen2.5-7B-Instruct]`
   - `hardware = [{type:"TEE", count:1}]`
   - `host = 127.0.0.1`
   - `port = 18180`

### 3) Run dAPI

- Run transfer dAPI (client-facing).
- Run executor dAPI on executor host with chain key for that participant.

Both use standard dAPI startup; no special global mode switch is required.

### 4) Send encrypted request

```bash
cd decentralized-api
go run ./cmd/openai-tee-connector \
  --dapi-url http://127.0.0.1:8080 \
  --model Qwen/Qwen2.5-7B-Instruct \
  --executor-id <participant_address_or_participant/local_id> \
  --message "Explain TEE encrypted inference in one paragraph"
```

## Test checklist

1. `reserve` returns:
   - `assignment.executor_id`
   - `assignment.executor_url`
   - `assignment.node_public_key`
2. connector receives decrypted model response.
3. response includes `X-Inference-Id` header (connector prints it).
4. chain shows both `StartInference` and `FinishInference` for that `inference_id`.
   - example query: `inferenced query inference show-inference <inference_id>`
5. repeating with another `executor_id` routes to another executor.

## API contract (TEE path)

- `POST /v1/tee/sessions/reserve`
  - input:
    - `model`, `max_tokens`
    - optional: `requester_address`, `user_id`, `executor_id`
  - output:
    - `session_id`
    - `assignment.executor_id`
    - `assignment.executor_url`
    - `assignment.node_public_key`
    - `assignment.node_url`

- `POST /v1/tee/chat/completions`
  - input:
    - `session_id`, `ephemeral_public_key`, `nonce`, `ciphertext`, optional `aad`
    - optional advanced fields: `request_timestamp`, `developer_signature`, `prompt_hash`, `original_prompt_hash`, `requester_address`
  - behavior:
    - transfer dAPI creates start tx, then relays encrypted body to executor dAPI
  - note:
    - one session maps to one inference request (subsequent calls on same session are rejected)
    - if `requester_address` differs from transfer dAPI address, `developer_signature` must be provided

- `POST /v1/tee/sessions/close`
  - closes local routing session object (billing is already handled by start/finish tx lifecycle)

## Deferred hardening (next steps)

- Attestation quote verification in reserve path.
- Signed usage receipts from TEE adapter and strict verification before finish settlement.
- Strong policy checks tying chain TEE metadata to runtime measurements.
