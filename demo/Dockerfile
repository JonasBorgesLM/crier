# Builds crierd for the demo.
#
# The build stage uses a supported Go toolchain rather than the library floor,
# per NFR2's second policy: nobody imports a binary, and the toolchain that
# builds it is the standard library that ships inside it.
FROM golang:1.27-alpine AS build

WORKDIR /src
COPY . .

RUN cd cmd/crierd \
 && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=demo" \
      -o /out/crierd .

FROM alpine:3.21

# wget is here for the container healthcheck, which reads /readyz.
RUN apk add --no-cache wget ca-certificates

COPY --from=build /out/crierd /usr/local/bin/crierd

# Not root: the demo should not model something nobody should copy.
RUN adduser -D -u 10001 crier
USER crier

ENTRYPOINT ["/usr/local/bin/crierd"]
