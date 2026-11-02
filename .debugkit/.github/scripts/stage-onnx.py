"""Stage verified native runtime files beside kennembed, including notices."""
import argparse
import hashlib
import os
import pathlib
import platform
import shutil
import subprocess
import tarfile
import urllib.request
import zipfile

targets = {
    ('linux', 'amd64'): ('linux-x64', 'tgz', '1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b'),
    ('linux', 'arm64'): ('linux-aarch64', 'tgz', '7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c'),
    ('darwin', 'amd64'): ('osx-x86_64', 'tgz', 'd10359e16347b57d9959f7e80a225a5b4a66ed7d7e007274a15cae86836485a6'),
    ('darwin', 'arm64'): ('osx-arm64', 'tgz', 'b4d513ab2b26f088c66891dbbc1408166708773d7cc4163de7bdca0e9bbb7856'),
    ('windows', 'amd64'): ('win-x64', 'zip', '0b38df9af21834e41e73d602d90db5cb06dbd1ca618948b8f1d66d607ac9f3cd'),
    ('windows', 'arm64'): ('win-arm64', 'zip', '1cfe88b6435df3b5fb0e9f6bd7d6f5df1e887b6174de7f6e2a47bab956f3f168'),
}
parser = argparse.ArgumentParser()
parser.add_argument('--os', default=platform.system().lower())
parser.add_argument('--arch', default='arm64' if platform.machine().lower() in ('arm64', 'aarch64') else 'amd64')
parser.add_argument('--output', type=pathlib.Path, required=True)
args = parser.parse_args()
target, suffix, digest = targets[(args.os, args.arch)]
root = args.output
root.mkdir(parents=True, exist_ok=True)
name = 'onnxruntime-' + target + '-1.23.2'
archive = root.parent / (name + '.' + suffix)
if not archive.exists():
    urllib.request.urlretrieve('https://github.com/microsoft/onnxruntime/releases/download/v1.23.2/' + archive.name, archive)
if hashlib.sha256(archive.read_bytes()).hexdigest() != digest:
    raise RuntimeError('ONNX Runtime SHA-256 mismatch')
# Copy regular library bytes, including versioned files, rather than archive symlinks.
if suffix == 'zip':
    with zipfile.ZipFile(archive) as source:
        for entry in source.infolist():
            path = pathlib.PurePosixPath(entry.filename)
            if path.suffix == '.dll' or path.name.lower().startswith(('license', 'thirdparty', 'notice')):
                (root / path.name).write_bytes(source.read(entry))
else:
    with tarfile.open(archive) as source:
        for entry in source:
            path = pathlib.PurePosixPath(entry.name)
            if entry.isfile() and ('.so' in path.name or path.suffix == '.dylib' or path.name.lower().startswith(('license', 'thirdparty', 'notice'))):
                with source.extractfile(entry) as data:
                    (root / path.name).write_bytes(data.read())
if args.os == 'windows':
    # Use the licensed Visual Studio redistribution files, never System32 copies.
    vswhere = pathlib.Path(os.environ['ProgramFiles(x86)']) / 'Microsoft Visual Studio/Installer/vswhere.exe'
    installation = subprocess.check_output([str(vswhere), '-latest', '-products', '*', '-property', 'installationPath'], text=True).strip()
    architecture = 'x64' if args.arch == 'amd64' else 'arm64'
    candidates = sorted((pathlib.Path(installation) / 'VC/Redist/MSVC').glob('*/' + architecture + '/Microsoft.VC*.CRT'))
    if not candidates:
        raise RuntimeError('Visual C++ redistribution files are missing for ' + architecture)
    for dll in ('msvcp140.dll', 'msvcp140_1.dll', 'vcruntime140.dll', 'vcruntime140_1.dll'):
        shutil.copy2(candidates[-1] / dll, root / dll)
    urllib.request.urlretrieve('https://aka.ms/VCRedistLicense', root / 'Visual-C++-Runtime-License.html')
print(next(path for path in root.iterdir() if path.name.startswith(('onnxruntime.', 'libonnxruntime.'))))
