# SIP media resources

See https://trac.ffmpeg.org/wiki/audio%20types.

## PCM16LE

Encoding:
```shell
ffmpeg -i file.ogg -ar 8000 -f s16le file.s16le
```

Decoding:
```shell
ffmpeg -f s16le -ar 8000 -i file.s16le file.ogg
```

## G711 - PCMU

Encoding:
```shell
ffmpeg -i file.ogg -ar 8000 -f mulaw file.g711u
```

Decoding:
```shell
ffmpeg -f mulaw -ar 8000 -i file.g711u file.ogg
```

## G711 - PCMA

Encoding:
```shell
ffmpeg -i file.ogg -ar 8000 -f alaw file.g711a
```

Decoding:
```shell
ffmpeg -f alaw -ar 8000 -i file.g711a file.ogg
```

## G722

Encoding:
```shell
ffmpeg -i file.ogg -ar 8000 -f g722 file.g722
```

Decoding:
```shell
ffmpeg -f g722 -i file.g722 file.ogg
```