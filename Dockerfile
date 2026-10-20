FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/batch ./cmd/batch

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/batch /batch
ENTRYPOINT ["/batch"]
CMD ["bill"]
