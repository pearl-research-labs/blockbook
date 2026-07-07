# Blockbook runtime image: the binary + static assets, nothing else.
# Network-independent — the blockchaincfg is supplied at run time via
# -blockchaincfg. Context is assembled by .github/workflows/release-image.yml.

FROM blockbook-build:latest

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# blockbook resolves ./static relative to the working directory.
WORKDIR /src

COPY static /src/static
COPY blockbook /src/blockbook

RUN chmod +x /src/blockbook

ENTRYPOINT ["/src/blockbook"]
