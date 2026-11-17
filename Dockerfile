FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETPLATFORM
USER nonroot:nonroot
ENTRYPOINT ["/atc", "server"]
COPY $TARGETPLATFORM/atc /