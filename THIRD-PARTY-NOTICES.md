# Third-Party Notices

`exiftool-mcp-go` is licensed under Apache License 2.0; see the repository's
[`LICENSE`](LICENSE). The notices below cover upstream code dependencies and
the Go runtime. The listed module versions come from this project's `go.mod`;
the exact packages linked into a particular binary depend on the build.

## ExifTool — external runtime dependency

**ExifTool**, by **Phil Harvey**. **Copyright 2003–2026, Phil Harvey.**
ExifTool is free software distributed under the same terms as Perl itself
(**the Perl Artistic License or the GNU General Public License**).

Source and licensing information: https://exiftool.org/ and
https://github.com/exiftool/exiftool.

**ExifTool is not bundled, copied, or statically linked into
`exiftool-mcp-go`.** Users install ExifTool separately; this MCP server runs
its command-line executable. ExifTool's own distribution carries its
applicable licenses. If a release bundles ExifTool in the future, that release
must also include the applicable ExifTool license text and notices.

## Go dependencies

| Component | Version | License |
| --- | --- | --- |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | Apache-2.0 and MIT (as applicable to code contributions) |
| `github.com/google/jsonschema-go` | v0.4.3 | MIT |
| `github.com/segmentio/asm` | v1.1.3 | MIT |
| `github.com/segmentio/encoding` | v0.5.4 | MIT |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause |
| `golang.org/x/oauth2` | v0.35.0 | BSD-3-Clause |
| `golang.org/x/sync` | v0.20.0 | BSD-3-Clause; Go PATENTS grant |
| `golang.org/x/sys` | v0.41.0 | BSD-3-Clause; Go PATENTS grant |
| `golang.org/x/time` | v0.15.0 | BSD-3-Clause; Go PATENTS grant |
| Go standard library and runtime | build toolchain (Go 1.25 series targeted) | BSD-3-Clause; Go PATENTS grant |

`go-sdk` is a direct dependency; the other modules are recorded as indirect
in `go.mod`. The MCP Go SDK's source is undergoing an Apache-2.0 / MIT
relicensing transition. Applicable Apache-2.0 terms are supplied in the
repository's `LICENSE`; applicable MIT terms are reproduced below.
This project does not redistribute the SDK's documentation.

## MIT License

Applies to `github.com/modelcontextprotocol/go-sdk` (MIT-licensed code
contributions), `github.com/google/jsonschema-go`,
`github.com/segmentio/asm`, and `github.com/segmentio/encoding`.

Copyright (c) 2024-2025 Model Context Protocol a Series of LF Projects, LLC.

Copyright (c) 2025 JSON Schema Go Project Authors

Copyright (c) 2021 Segment

Copyright (c) 2019 Segment.io, Inc.

```text
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## Go Authors — BSD 3-Clause License

Applies to the Go standard library and runtime, `golang.org/x/oauth2`,
`golang.org/x/sync`, `golang.org/x/sys`, and `golang.org/x/time`.

```text
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

 * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
 * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
 * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

### Go additional patent grant

Applies to the Go standard library and runtime, `golang.org/x/sync`,
`golang.org/x/sys`, and `golang.org/x/time`.

```text
Additional IP Rights Grant (Patents)

"This implementation" means the copyrightable works distributed by
Google as part of the Go project.

Google hereby grants to You a perpetual, worldwide, non-exclusive,
no-charge, royalty-free, irrevocable (except as stated in this section)
patent license to make, have made, use, offer to sell, sell, import,
transfer and otherwise run, modify and propagate the contents of this
implementation of Go, where such license applies only to those patent
claims, both currently owned or controlled by Google and acquired in
the future, licensable by Google that are necessarily infringed by this
implementation of Go. This grant does not include claims that would be
infringed only as a consequence of further modification of this
implementation. If you or your agent or exclusive licensee institute or
order or agree to the institution of patent litigation against any
entity (including a cross-claim or counterclaim in a lawsuit) alleging
that this implementation of Go or any code incorporated within this
implementation of Go constitutes direct or contributory patent
infringement, or inducement of patent infringement, then any patent
rights granted to you under this License for this implementation of Go
shall terminate as of the date such litigation is filed.
```

## uritemplate — BSD 3-Clause License

Applies to `github.com/yosida95/uritemplate/v3`.

```text
Copyright (C) 2016, Kohei YOSHIDA. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

 * Redistributions of source code must retain the above copyright
 notice, this list of conditions and the following disclaimer.
 * Redistributions in binary form must reproduce the above copyright
 notice, this list of conditions and the following disclaimer in the
 documentation and/or other materials provided with the distribution.
 * Neither the name of the copyright holder nor the names of its
 contributors may be used to endorse or promote products derived from
 this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
