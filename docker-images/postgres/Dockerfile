# pg_textsearch adds real BM25 ranking (IDF, term-frequency saturation,
# document-length normalisation) where Postgres's built-in ts_rank_cd has
# none — see docs/retrieval-design.md. It ships no image and no apt package,
# only tagged source, so this layers it onto the same image the chart already
# deploys rather than switching Postgres distributions.
FROM ghcr.io/cloudnative-pg/postgresql:18-standard-trixie

ARG PG_TEXTSEARCH_VERSION=v1.3.1

USER root

# postgresql-server-dev-18 comes from the pgdg apt repo this image already has
# configured, pinned to the same 18.x build as the server itself — so
# pg_config resolves to headers that actually match what's running, not
# whatever Debian's own postgres package would ship.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
         build-essential \
         postgresql-server-dev-18 \
         git \
         ca-certificates \
    && git clone --branch "${PG_TEXTSEARCH_VERSION}" --depth 1 \
         https://github.com/timescale/pg_textsearch.git /tmp/pg_textsearch \
    && make -C /tmp/pg_textsearch \
    && make -C /tmp/pg_textsearch install \
    && apt-get purge -y --auto-remove build-essential git \
    && rm -rf /tmp/pg_textsearch /var/lib/apt/lists/*

USER postgres
