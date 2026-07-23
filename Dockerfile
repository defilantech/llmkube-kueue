FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /llmkube-kueue ./cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /llmkube-kueue /llmkube-kueue
USER 65532:65532
ENTRYPOINT ["/llmkube-kueue"]
