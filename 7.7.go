// Copyright 2019 Google LLC
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file or at
// https://developers.google.com/open-source/licenses/bsd

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
	"golang.org/x/term"
)

type multiFlag []string

func (f *multiFlag) String() string { return fmt.Sprint(*f) }

func (f *multiFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

const usage = `Usage:
    age [--encrypt] (-r RECIPIENT | -R PATH)... [--armor] [-o OUTPUT] [INPUT]
    age [--encrypt] --passphrase [--armor] [-o OUTPUT] [INPUT]
    age --decrypt [-i PATH]... [-o OUTPUT] [INPUT]

Options:
    -e, --encrypt               Encrypt the input to the output. Default if omitted.
    -d, --decrypt               Decrypt the input to the output.
    -o, --output OUTPUT         Write the result to the file at path OUTPUT.
    -a, --armor                 Encrypt to a PEM encoded format.
    -p, --passphrase            Encrypt with a passphrase.
    -r, --recipient RECIPIENT   Encrypt to the specified RECIPIENT. Can be repeated.
    -R, --recipients-file PATH  Encrypt to recipients listed at PATH. Can be repeated.
    -i, --identity PATH         Use the identity file at PATH. Can be repeated.

INPUT defaults to standard input, and OUTPUT defaults to standard output.
If OUTPUT exists, it will be overwritten.

RECIPIENT can be an age public key generated by age-keygen ("age1...")
or an SSH public key ("ssh-ed25519 AAAA...", "ssh-rsa AAAA...").

Recipient files contain one or more recipients, one per line. Empty lines
and lines starting with "#" are ignored as comments. "-" may be used to
read recipients from standard input.

Identity files contain one or more secret keys ("AGE-SECRET-KEY-1..."),
one per line, or an SSH key. Empty lines and lines starting with "#" are
ignored as comments. Passphrase encrypted age files can be used as
identity files. Multiple key files can be provided, and any unused ones
will be ignored. "-" may be used to read identities from standard input.

When --encrypt is specified explicitly, -i can also be used to encrypt to an
identity file symmetrically, instead or in addition to normal recipients.

Example:
    $ age-keygen -o key.txt
    Public key: age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
    $ tar cvz ~/data | age -r age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p > data.tar.gz.age
    $ age --decrypt -i key.txt -o data.tar.gz data.tar.gz.age`

// Version can be set at link time to override debug.BuildInfo.Main.Version,
// which is "(devel)" when building from within the module. See
// golang.org/issue/29814 and golang.org/issue/29228.
var Version string

func main() {
	log.SetFlags(0)
	flag.Usage = func() { fmt.Fprintf(os.Stderr, "%s\n", usage) }

	if len(os.Args) == 1 {
		flag.Usage()
		os.Exit(1)
	}

	var (
		outFlag                          string
		decryptFlag, encryptFlag         bool
		passFlag, versionFlag, armorFlag bool
		recipientFlags, identityFlags    multiFlag
		recipientsFileFlags              multiFlag
	)

	flag.BoolVar(&versionFlag, "version", false, "print the version")
	flag.BoolVar(&decryptFlag, "d", false, "decrypt the input")
	flag.BoolVar(&decryptFlag, "decrypt", false, "decrypt the input")
	flag.BoolVar(&encryptFlag, "e", false, "encrypt the input")
	flag.BoolVar(&encryptFlag, "encrypt", false, "encrypt the input")
	flag.BoolVar(&passFlag, "p", false, "use a passphrase")
	flag.BoolVar(&passFlag, "passphrase", false, "use a passphrase")
	flag.StringVar(&outFlag, "o", "", "output to `FILE` (default stdout)")
	flag.StringVar(&outFlag, "output", "", "output to `FILE` (default stdout)")
	flag.BoolVar(&armorFlag, "a", false, "generate an armored file")
	flag.BoolVar(&armorFlag, "armor", false, "generate an armored file")
	flag.Var(&recipientFlags, "r", "recipient (can be repeated)")
	flag.Var(&recipientFlags, "recipient", "recipient (can be repeated)")
	flag.Var(&recipientsFileFlags, "R", "recipients file (can be repeated)")
	flag.Var(&recipientsFileFlags, "recipients-file", "recipients file (can be repeated)")
	flag.Var(&identityFlags, "i", "identity (can be repeated)")
	flag.Var(&identityFlags, "identity", "identity (can be repeated)")
	flag.Parse()

	if versionFlag {
		if Version != "" {
			fmt.Println(Version)
			return
		}
		if buildInfo, ok := debug.ReadBuildInfo(); ok {
			fmt.Println(buildInfo.Main.Version)
			return
		}
		fmt.Println("(unknown)")
		return
	}

	if flag.NArg() > 1 {
		errorWithHint(fmt.Sprintf("too many arguments: %q", flag.Args()),
			"note that the input file must be specified after all flags")
	}
	switch {
	case decryptFlag:
		if encryptFlag {
			errorf("-e/--encrypt can't be used with -d/--decrypt")
		}
		if armorFlag {
			errorWithHint("-a/--armor can't be used with -d/--decrypt",
				"note that armored files are detected automatically")
		}
		if passFlag {
			errorWithHint("-p/--passphrase can't be used with -d/--decrypt",
				"note that password protected files are detected automatically")
		}
		if len(recipientFlags) > 0 {
			errorWithHint("-r/--recipient can't be used with -d/--decrypt",
				"did you mean to use -i/--identity to specify a private key?")
		}
		if len(recipientsFileFlags) > 0 {
			errorWithHint("-R/--recipients-file can't be used with -d/--decrypt",
				"did you mean to use -i/--identity to specify a private key?")
		}
	default: // encrypt
		if len(identityFlags) > 0 && !encryptFlag {
			errorWithHint("-i/--identity can't be used in encryption mode unless symmetric encryption is explicitly selected with -e/--encrypt",
				"did you forget to specify -d/--decrypt?")
		}
		if len(recipientFlags)+len(recipientsFileFlags)+len(identityFlags) == 0 && !passFlag {
			errorWithHint("missing recipients",
				"did you forget to specify -r/--recipient, -R/--recipients-file or -p/--passphrase?")
		}
		if len(recipientFlags) > 0 && passFlag {
			errorf("-p/--passphrase can't be combined with -r/--recipient")
		}
		if len(recipientsFileFlags) > 0 && passFlag {
			errorf("-p/--passphrase can't be combined with -R/--recipients-file")
		}
		if len(identityFlags) > 0 && passFlag {
			errorf("-p/--passphrase can't be combined with -i/--identity")
		}
	}

	var in io.Reader = os.Stdin
	var out io.Writer = os.Stdout
	if name := flag.Arg(0); name != "" && name != "-" {
		f, err := os.Open(name)
		if err != nil {
			errorf("failed to open input file %q: %v", name, err)
		}
		defer f.Close()
		in = f
	} else {
		stdinInUse = true
	}
	if name := outFlag; name != "" && name != "-" {
		f := newLazyOpener(name)
		defer func() {
			if err := f.Close(); err != nil {
				errorf("failed to close output file %q: %v", name, err)
			}
		}()
		out = f
	} else if term.IsTerminal(int(os.Stdout.Fd())) {
		if name != "-" {
			if decryptFlag {
				// TODO: buffer the output and check it's printable.
			} else if !armorFlag {
				// If the output wouldn't be armored, refuse to send binary to
				// the terminal unless explicitly requested with "-o -".
				errorWithHint("refusing to output binary to the terminal",
					"did you mean to use -a/--armor?",
					`force anyway with "-o -"`)
			}
		}
		if in == os.Stdin && term.IsTerminal(int(os.Stdin.Fd())) {
			// If the input comes from a TTY and output will go to a TTY,
			// buffer it up so it doesn't get in the way of typing the input.
			buf := &bytes.Buffer{}
			defer func() { io.Copy(os.Stdout, buf) }()
			out = buf
		}
	}

	switch {
	case decryptFlag:
		decrypt(identityFlags, in, out)
	case passFlag:
		pass, err := passphrasePromptForEncryption()
		if err != nil {
			errorf("%v", err)
		}
		encryptPass(pass, in, out, armorFlag)
	default:
		encryptKeys(recipientFlags, recipientsFileFlags, identityFlags, in, out, armorFlag)
	}
}

func passphrasePromptForEncryption() (string, error) {
	pass, err := readPassphrase("Enter passphrase (leave empty to autogenerate a secure one):")
	if err != nil {
		return "", fmt.Errorf("could not read passphrase: %v", err)
	}
	p := string(pass)
	if p == "" {
		var words []string
		for i := 0; i < 10; i++ {
			words = append(words, randomWord())
		}
		p = strings.Join(words, "-")
		// TODO: consider printing this to the terminal, instead of stderr.
		fmt.Fprintf(os.Stderr, "Using the autogenerated passphrase %q.\n", p)
	} else {
		confirm, err := readPassphrase("Confirm passphrase:")
		if err != nil {
			return "", fmt.Errorf("could not read passphrase: %v", err)
		}
		if string(confirm) != p {
			return "", fmt.Errorf("passphrases didn't match")
		}
	}
	return p, nil
}

func encryptKeys(keys, files, identities []string, in io.Reader, out io.Writer, armor bool) {
	var recipients []age.Recipient
	for _, arg := range keys {
		r, err := parseRecipient(arg)
		if err, ok := err.(gitHubRecipientError); ok {
			errorWithHint(err.Error(), "instead, use recipient files like",
				"    curl -O https://github.com/"+err.username+".keys",
				"    age -R "+err.username+".keys")
		}
		if err != nil {
			errorf("%v", err)
		}
		recipients = append(recipients, r)
	}
	for _, name := range files {
		recs, err := parseRecipientsFile(name)
		if err != nil {
			errorf("failed to parse recipient file %q: %v", name, err)
		}
		recipients = append(recipients, recs...)
	}
	for _, name := range identities {
		ids, err := parseIdentitiesFile(name)
		if err != nil {
			errorf("reading %q: %v", name, err)
		}
		r, err := identitiesToRecipients(ids)
		if err != nil {
			errorf("internal error processing %q: %v", name, err)
		}
		recipients = append(recipients, r...)
	}
	encrypt(recipients, in, out, armor)
}

func encryptPass(pass string, in io.Reader, out io.Writer, armor bool) {
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		errorf("%v", err)
	}
	encrypt([]age.Recipient{r}, in, out, armor)
}

func encrypt(recipients []age.Recipient, in io.Reader, out io.Writer, withArmor bool) {
	if withArmor {
		a := armor.NewWriter(out)
		defer func() {
			if err := a.Close(); err != nil {
				errorf("%v", err)
			}
		}()
		out = a
	}
	w, err := age.Encrypt(out, recipients...)
	if err != nil {
		errorf("%v", err)
	}
	if _, err := io.Copy(w, in); err != nil {
		errorf("%v", err)
	}
	if err := w.Close(); err != nil {
		errorf("%v", err)
	}
}

func decrypt(keys []string, in io.Reader, out io.Writer) {
	identities := []age.Identity{
		// If there is an scrypt recipient (it will have to be the only one and)
		// this identity will be invoked.
		&LazyScryptIdentity{passphrasePrompt},
	}

	for _, name := range keys {
		ids, err := parseIdentitiesFile(name)
		if err != nil {
			errorf("reading %q: %v", name, err)
		}
		identities = append(identities, ids...)
	}

	rr := bufio.NewReader(in)
	if start, _ := rr.Peek(len(armor.Header)); string(start) == armor.Header {
		in = armor.NewReader(rr)
	} else {
		in = rr
	}

	r, err := age.Decrypt(in, identities...)
	if err != nil {
		errorf("%v", err)
	}
	if _, err := io.Copy(out, r); err != nil {
		errorf("%v", err)
	}
}

func passphrasePrompt() (string, error) {
	pass, err := readPassphrase("Enter passphrase:")
	if err != nil {
		return "", fmt.Errorf("could not read passphrase: %v", err)
	}
	return string(pass), nil
}

func identitiesToRecipients(ids []age.Identity) ([]age.Recipient, error) {
	var recipients []age.Recipient
	for _, id := range ids {
		switch id := id.(type) {
		case *age.X25519Identity:
			recipients = append(recipients, id.Recipient())
		case *agessh.RSAIdentity:
			recipients = append(recipients, id.Recipient())
		case *agessh.Ed25519Identity:
			recipients = append(recipients, id.Recipient())
		case *agessh.EncryptedSSHIdentity:
			recipients = append(recipients, id.Recipient())
		case *EncryptedIdentity:
			r, err := id.Recipients()
			if err != nil {
				return nil, err
			}
			recipients = append(recipients, r...)
		default:
			return nil, fmt.Errorf("unexpected identity type: %T", id)
		}
	}
	return recipients, nil
}

type lazyOpener struct {
	name string
	f    *os.File
	err  error
}

func newLazyOpener(name string) io.WriteCloser {
	return &lazyOpener{name: name}
}

func (l *lazyOpener) Write(p []byte) (n int, err error) {
	if l.f == nil && l.err == nil {
		l.f, l.err = os.Create(l.name)
	}
	if l.err != nil {
		return 0, l.err
	}
	return l.f.Write(p)
}

func (l *lazyOpener) Close() error {
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}

func errorf(format string, v ...interface{}) {
	log.Printf("age: error: "+format, v...)
	log.Fatalf("age: report unexpected or unhelpful errors at https://filippo.io/age/report")
}

func warningf(format string, v ...interface{}) {
	log.Printf("age: warning: "+format, v...)
}

func errorWithHint(error string, hints ...string) {
	log.Printf("age: error: %s", error)
	for _, hint := range hints {
		log.Printf("age: hint: %s", hint)
	}
	log.Fatalf("age: report unexpected or unhelpful errors at https://filippo.io/age/report")
}