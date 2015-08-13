package engine

import (
	"bytes"

	"github.com/agl/ed25519"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/scrypt"

	"github.com/keybase/client/go/libkb"
)

type BackupKeygenArg struct {
	Passphrase string
	SkipPush   bool
	Me         *libkb.User
	SigningKey libkb.GenericKey
}

// BackupKeygen is an engine.
type BackupKeygen struct {
	arg    *BackupKeygenArg
	sigKey libkb.GenericKey
	encKey libkb.GenericKey
	libkb.Contextified
}

// NewBackupKeygen creates a BackupKeygen engine.
func NewBackupKeygen(arg *BackupKeygenArg, g *libkb.GlobalContext) *BackupKeygen {
	return &BackupKeygen{
		arg:          arg,
		Contextified: libkb.NewContextified(g),
	}
}

// Name is the unique engine name.
func (e *BackupKeygen) Name() string {
	return "BackupKeygen"
}

// GetPrereqs returns the engine prereqs.
func (e *BackupKeygen) Prereqs() Prereqs {
	// only need session if pushing keys
	return Prereqs{
		Session: !e.arg.SkipPush,
	}
}

// RequiredUIs returns the required UIs.
func (e *BackupKeygen) RequiredUIs() []libkb.UIKind {
	return []libkb.UIKind{}
}

// SubConsumers returns the other UI consumers for this engine.
func (e *BackupKeygen) SubConsumers() []libkb.UIConsumer {
	return []libkb.UIConsumer{
		&DetKeyEngine{},
	}
}

func (e *BackupKeygen) SigKey() libkb.GenericKey {
	return e.sigKey
}

func (e *BackupKeygen) EncKey() libkb.GenericKey {
	return e.encKey
}

// Run starts the engine.
func (e *BackupKeygen) Run(ctx *Context) error {
	// make the passphrase stream
	key, err := scrypt.Key([]byte(e.arg.Passphrase), nil,
		libkb.BackupKeyScryptCost, libkb.BackupKeyScryptR, libkb.BackupKeyScryptP, libkb.BackupKeyScryptKeylen)
	if err != nil {
		return err
	}

	ppStream := libkb.NewPassphraseStream(key)

	// make keys for the backup device
	if err := e.makeSigKey(ppStream.EdDSASeed()); err != nil {
		return err
	}
	if err := e.makeEncKey(ppStream.DHSeed()); err != nil {
		return err
	}

	// push everything to the server
	if err := e.push(ctx); err != nil {
		return err
	}

	return nil
}

func (e *BackupKeygen) makeSigKey(seed []byte) error {
	pub, priv, err := ed25519.GenerateKey(bytes.NewBuffer(seed))
	if err != nil {
		return err
	}

	var key libkb.NaclSigningKeyPair
	copy(key.Public[:], (*pub)[:])
	key.Private = &libkb.NaclSigningKeyPrivate{}
	copy(key.Private[:], (*priv)[:])

	e.sigKey = key

	return nil
}

func (e *BackupKeygen) makeEncKey(seed []byte) error {
	pub, priv, err := box.GenerateKey(bytes.NewBuffer(seed))
	if err != nil {
		return err
	}
	var key libkb.NaclDHKeyPair
	copy(key.Public[:], (*pub)[:])
	key.Private = &libkb.NaclDHKeyPrivate{}
	copy(key.Private[:], (*priv)[:])

	e.encKey = key

	return nil
}

func (e *BackupKeygen) push(ctx *Context) error {
	if e.arg.SkipPush {
		return nil
	}

	// create a new backup device
	dev, err := libkb.NewBackupDevice()
	if err != nil {
		return err
	}

	// create lks halves for this device.  Note that they aren't used for
	// local, encrypted storage of the backup keys, but just for recovery
	// purposes.

	var ppgen libkb.PassphraseGeneration
	var clientHalf []byte
	e.G().LoginState().Account(func(a *libkb.Account) {
		ppgen = a.PassphraseStreamCache().PassphraseStream().Generation()
		clientHalf = a.PassphraseStreamCache().PassphraseStream().LksClientHalf()
	}, "BackupKeygen - push")

	lks := libkb.NewLKSec(clientHalf, ppgen, e.arg.Me.GetUID(), e.G())
	if err := lks.GenerateServerHalf(); err != nil {
		return err
	}
	ctext, err := lks.EncryptClientHalfRecovery(e.encKey)
	if err != nil {
		return err
	}

	// post them to the server.
	if err := libkb.PostDeviceLKS(ctx.LoginContext, dev.ID, libkb.DeviceTypeBackup, lks.GetServerHalf(), lks.Generation(), ctext, e.encKey.GetKID()); err != nil {
		return err
	}

	// push the backup signing key
	sigDel := libkb.Delegator{
		NewKey:      e.sigKey,
		Sibkey:      true,
		Expire:      libkb.NaclEdDSAExpireIn,
		ExistingKey: e.arg.SigningKey,
		Me:          e.arg.Me,
		Device:      dev,
	}
	if err := sigDel.Run(ctx.LoginContext); err != nil {
		return err
	}

	// push the backup encryption key
	sigEnc := libkb.Delegator{
		NewKey:      e.encKey,
		Sibkey:      false,
		Expire:      libkb.NaclDHExpireIn,
		ExistingKey: e.sigKey,
		Me:          e.arg.Me,
		Device:      dev,
	}
	if err := sigEnc.Run(ctx.LoginContext); err != nil {
		return err
	}

	return nil
}
