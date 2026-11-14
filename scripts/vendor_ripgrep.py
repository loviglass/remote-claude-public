"""Pinned upstream ripgrep assets, verified before extracting selected files."""
import hashlib
import pathlib
import tarfile
import urllib.request
import zipfile

VERSION = '15.2.0'
ASSETS = {
    ('linux', 'amd64'): ('x86_64-unknown-linux-musl.tar.gz', '33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c'),
    ('linux', 'arm64'): ('aarch64-unknown-linux-gnu.tar.gz', 'a740b91c82eaf9914cfedd353572f2791cbe0162c84101ee0951058f4dcbc90d'),
    ('darwin', 'amd64'): ('x86_64-apple-darwin.tar.gz', 'af7825fcc69a2afc7a7aea55fc9af90e26421d8f20fe59df32e233c0b8a231c1'),
    ('darwin', 'arm64'): ('aarch64-apple-darwin.tar.gz', '3750b2e93f37e0c692657da574d7019a101c0084da05a790c83fd335bad973e4'),
    ('windows', 'amd64'): ('x86_64-pc-windows-msvc.zip', '71b2fef860abe467217a538ff31de02f5258807c0129f771846f87bd029aafc5'),
    ('windows', 'arm64'): ('aarch64-pc-windows-msvc.zip', 'e4abca10c3a64ebea742667dd7009449d49403db5460dd6873e389fa2945360f'),
}

def bundle(system, arch, stage, cache):
    suffix, digest = ASSETS[system, arch]
    name = f'ripgrep-{VERSION}-{suffix}'
    cache.mkdir(parents=True, exist_ok=True)
    archive = cache / name
    if not archive.exists():
        with urllib.request.urlopen(f'https://github.com/BurntSushi/ripgrep/releases/download/{VERSION}/{name}', timeout=60) as response:
            data = response.read()
        if hashlib.sha256(data).hexdigest() != digest:
            raise ValueError(f'ripgrep checksum mismatch: {name}')
        archive.write_bytes(data)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != digest:
        raise ValueError(f'ripgrep checksum mismatch: {archive}')
    binary = 'rg.exe' if system == 'windows' else 'rg'
    target = 'remote-claude-rg.exe' if system == 'windows' else 'remote-claude-rg'
    names = {binary: target, 'COPYING': 'RIPGREP-COPYING', 'LICENSE-MIT': 'RIPGREP-LICENSE-MIT', 'UNLICENSE': 'RIPGREP-UNLICENSE'}
    members = []
    if name.endswith('.zip'):
        with zipfile.ZipFile(archive) as z:
            for entry in z.namelist():
                leaf = pathlib.PurePosixPath(entry).name
                if leaf in names:
                    p = stage / names[leaf]; p.write_bytes(z.read(entry)); members.append(p)
    else:
        with tarfile.open(archive) as tar:
            for entry in tar.getmembers():
                leaf = pathlib.PurePosixPath(entry.name).name
                if entry.isfile() and leaf in names:
                    p = stage / names[leaf]; p.write_bytes(tar.extractfile(entry).read()); members.append(p)
    if stage / target not in members or stage / 'RIPGREP-COPYING' not in members:
        raise ValueError(f'Missing binary or license in {name}')
    (stage / target).chmod(0o755)
    return members
