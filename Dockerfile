# omnia-dialhome: a PromptKit pack served to Omnia over the dial-home channel.
#
# The image holds the binary only. Give it a pack by mounting one, or by
# building FROM this image and copying yours in, and point it there:
#
#   docker run -v $PWD/agent.pack.json:/pack/agent.pack.json \
#     -e OMNIA_DIALHOME_PACK=/pack/agent.pack.json -e OMNIA_DIALHOME_PROMPT=chat \
#     -e OMNIA_DIAL_HOME_URL=wss://... -e OMNIA_DIAL_HOME_KEY=... omnia-dialhome

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/omnia-dialhome ./cmd/omnia-dialhome

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/omnia-dialhome /omnia-dialhome
USER nonroot:nonroot
ENTRYPOINT ["/omnia-dialhome"]
