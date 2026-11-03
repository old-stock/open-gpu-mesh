# llama.cpp CUDA server with a 0.5B model baked in (~0.5 GB VRAM). amd64-only base image, so
# the ogm scheduler only places it on x86 nodes automatically.
FROM ghcr.io/ggml-org/llama.cpp:server-cuda
ADD https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct-GGUF/resolve/main/qwen2.5-0.5b-instruct-q4_k_m.gguf /models/model.gguf
ENV LLAMA_ARG_MODEL=/models/model.gguf LLAMA_ARG_HOST=0.0.0.0 LLAMA_ARG_PORT=8080 \
    LLAMA_ARG_N_GPU_LAYERS=99 LLAMA_ARG_CTX_SIZE=2048 LLAMA_ARG_N_PARALLEL=1
EXPOSE 8080
