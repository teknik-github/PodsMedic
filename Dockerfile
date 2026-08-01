FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/podsmedic ./cmd/podsmedic

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/podsmedic /podsmedic
USER nonroot:nonroot
ENTRYPOINT ["/podsmedic"]
