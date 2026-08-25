FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/ags-slc/m8/cmd.version=${VERSION} -X github.com/ags-slc/m8/cmd.commit=${COMMIT} -X github.com/ags-slc/m8/cmd.date=${DATE}" \
    -o /m8 .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /m8 /usr/local/bin/m8
ENTRYPOINT ["m8"]

