FROM debian
COPY ./stonelb /stonelb
ENTRYPOINT ["/stonelb"]