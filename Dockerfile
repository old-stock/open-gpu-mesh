# Single image for both roles:
#   server: docker run -p 7777:7777 -v ogm:/data ghcr.io/old-stock/open-gpu-mesh server
#   agent:  docker run --gpus all -v /var/run/docker.sock:/var/run/docker.sock \
#             ghcr.io/old-stock/open-gpu-mesh agent --server https://gpu.example.com --token ogmn_...
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0-dev
RUN apk add --no-cache make && make dist VERSION=$VERSION PLATFORMS="linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64" \
 && cp dist/ogm-$(go env GOOS)-$(go env GOARCH) /ogm

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /ogm /usr/local/bin/ogm
COPY --from=build /src/dist /dist
ENV OGM_DATA=/data OGM_DIST_DIR=/dist OGM_LISTEN=:7777
VOLUME /data
EXPOSE 7777
ENTRYPOINT ["ogm"]
CMD ["server"]
