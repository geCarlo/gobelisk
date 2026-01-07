# gobelisk

## Dev Reqs

To build and release gobelisk you need:

- Go >= 1.22
- dpkg-deb
- aptly
- gpg (for repo signing)

These are build-time tools only.
They are not required on Raspberry Pi devices.

Go installation instructions:

```bash
wget https://go.dev/dl/go1.22.1.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.22.1.linux-arm64.tar.gz
export PATH=/usr/local/go/bin:$PATH
```