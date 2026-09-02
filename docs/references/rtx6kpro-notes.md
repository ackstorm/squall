# rtx6kpro — extracted facts (read 2026-09-01)

https://github.com/local-inference-lab/rtx6kpro — community field wiki for serving large
MoE LLMs on RTX PRO 6000 Blackwell (SM120, 96GB GDDR7), PCIe-only, no NVLink. The exact
card class the verified qwen sample rents on Vast.ai. Runbooks pin Docker digests, commits
and model snapshot ids; everything below is THEIR measurement, on overclocked lab boxes
(+6000MHz VRAM in the newest tables) — treat as ceilings, not promises.

Linked from README; feeds `config/samples/squall_v1alpha1_model.yaml` (perf-headroom
comment block) and `config/samples/squall_v1alpha1_model_glm53_flash.yaml`.

## Qwen3.8-27B (`models/qwen38-27b.md`) — our serving model

Best documented single-card vLLM path: official `Qwen/Qwen3.8-27B-FP8` (what we already
serve) + fp8 KV + FlashInfer + MTP3 + prefix caching, `max_num_seqs=4`, TP=1:

- Prefill ~7,686 tok/s @8k ctx, falling to ~4,439 @128k.
- Single-stream decode ~77.8 tok/s (69.8 @128k); 4-stream ~292.7.
- MTP3 alone: 106.9 vs 69.4 tok/s single-stream, 70–77% draft acceptance (~1.5x).
- **Attention backend is the big one at long context:** implicit backend collapsed
  126.7 → 31.9 tok/s at 128k (TP=2 table); explicit FlashInfer held 87.5+.
- All TP=1 rows are tagged `research-only` (single captures, no variance).
- Their MTP numbers come from a community vLLM build ("Gilded Gnosis r31"); stock-vLLM
  flag spelling for this checkpoint's MTP head is unverified by us.
- Trap: `lribeiro/Qwen3.8-27B-nvfp4-v17` is W8A8 FP8 GPTQ despite the `nvfp4` name.

## GLM-5.3-Flash (`models/glm-5.3-flash.md`, R12) — the 4-card sample

- Checkpoint `local-inference-lab/GLM-5.3-Flash-NVFP4` (ModelOpt NVFP4 W4A4, 288 experts,
  FP8-compressed MLA KV). Tracks HF `main` — pin `MODEL_REVISION` yourself.
- Image `voipmonitor/vllm:jovian-judgement-community-20260901-r12`
  = `voipmonitor/vllm@sha256:80dc3c3481255c123b3fe9ff164a879b7a141292389d29b0fd04a8472e6bf15d`.
  ENV-DRIVEN entrypoint (MODEL/PORT/TP/MTP/…), not stock vllm-openai — which is why the
  squall sample carries no spec.model/spec.args (nil engineCommands keeps the image CMD,
  and single-id /v1/models lets the proxy bridge the name, D100).
- Qualified only at TP=4 on 4x 96GB, 262,144 ctx. MTP3: ~14.8k tok/s prefill @32k,
  254.3 tok/s single-stream decode (vs 167.5 no-spec). DFlash2:7 wins only under load.
- On-disk weight size is NOT published; the sample's `disk: 400GB..` is a guess.
- LMCache optional (~-2.3% cold prefill, nothing on decode); never enable lazy-store
  for this model.

## Footguns worth carrying into spec.env review

- `NCCL_GRAPH_FILE=` set to EMPTY breaks NCCL outright — their top-billed footgun.
  Relevant to us: spec.env is verbatim passthrough, nothing would catch it.
- MTP-on and MTP-off throughput tables are not comparable (acceptance changes batch and
  CUDA-graph ladder); steps/s is their regression metric.
- PCIe-only TP needs the NCCL tuning block copied into the GLM sample
  (`NCCL_P2P_LEVEL=SYS`, `NCCL_PROTO=LL,LL128,Simple`, channel pinning, IB off).
