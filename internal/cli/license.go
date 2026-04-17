package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// writeLicenseFile writes a LICENSE file into targetPath based on the chosen
// license identifier. Supported values: "MIT", "Apache-2.0", "BSD-3-Clause",
// "none" (or empty). Unknown values return an error so typos fail loudly.
func writeLicenseFile(targetPath, license, author string) error {
	normalized := strings.ToLower(strings.TrimSpace(license))
	if normalized == "" || normalized == "none" {
		return nil
	}

	body, ok := renderLicenseBody(normalized, author)
	if !ok {
		return fmt.Errorf("unsupported --license value %q (supported: MIT, Apache-2.0, BSD-3-Clause, none)", license)
	}

	dest := filepath.Join(targetPath, "LICENSE")
	if err := os.WriteFile(dest, []byte(body), 0644); err != nil {
		return fmt.Errorf("write LICENSE: %w", err)
	}
	return nil
}

// renderLicenseBody returns the license text for the given identifier, or
// (empty, false) if the identifier is unknown.
func renderLicenseBody(id, author string) (string, bool) {
	year := fmt.Sprintf("%d", time.Now().Year())
	holder := author
	if holder == "" {
		holder = detectGitUserName()
	}
	if holder == "" {
		holder = "The Authors"
	}

	switch id {
	case "mit":
		return mitLicenseText(year, holder), true
	case "apache-2.0", "apache2", "apache":
		return apacheLicenseText(year, holder), true
	case "bsd-3-clause", "bsd3", "bsd":
		return bsd3LicenseText(year, holder), true
	default:
		return "", false
	}
}

// detectGitUserName reads `git config user.name` from the caller's environment.
// Returns "" if git isn't installed or the config isn't set.
func detectGitUserName() string {
	path, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	out, err := exec.Command(path, "config", "user.name").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func mitLicenseText(year, holder string) string {
	return fmt.Sprintf(`MIT License

Copyright (c) %s %s

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
`, year, holder)
}

func apacheLicenseText(year, holder string) string {
	return fmt.Sprintf(`                                 Apache License
                           Version 2.0, January 2004
                        http://www.apache.org/licenses/

Copyright %s %s

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

See http://www.apache.org/licenses/LICENSE-2.0 for the full license text.
`, year, holder)
}

func bsd3LicenseText(year, holder string) string {
	return fmt.Sprintf(`BSD 3-Clause License

Copyright (c) %s, %s
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice,
   this list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its
   contributors may be used to endorse or promote products derived from
   this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
POSSIBILITY OF SUCH DAMAGE.
`, year, holder)
}
