FROM golang:1.26 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -o /wto ./cmd/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /wto /wto
USER 65532:65532
ENTRYPOINT ["/wto"]
