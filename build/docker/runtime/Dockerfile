## Blockbook runtime image — environment-independent.
##
## Like upstream, this is a single image for all networks: it carries only the
## blockbook binary and its static assets. The blockchaincfg (coin params + RPC
## endpoint/creds) is provided at run time via `-blockchaincfg=<path>` by
## whatever spawns the container (e.g. a helm chart mounting it), not baked in.
##
## The Makefile's `build` target produces only the builder image and the binary
## (no runnable image), so this thin runtime layer lives here. It is assembled
## by .github/workflows/release-image.yml from a small context:
##   - blockbook  (the built binary, from build/blockbook)
##   - static/    (static web assets)

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
