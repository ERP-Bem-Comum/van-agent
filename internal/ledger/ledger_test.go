package ledger_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
)

const name = "PAG_000000.20260818120000_0001.REM"

var at = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func newLedger(t *testing.T) (*ledger.FileLedger, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ledger")
	led, err := ledger.NewFileLedger(dir)
	if err != nil {
		t.Fatalf("criar registro: %v", err)
	}
	return led, dir
}

func TestNomeDesconhecidoNaoTemRegistro(t *testing.T) {
	led, _ := newLedger(t)

	_, found, err := led.Lookup(name)
	if err != nil {
		t.Fatalf("consultar: %v", err)
	}
	if found {
		t.Error("não deveria haver registro para nome nunca visto")
	}
}

func TestIntencaoFicaLegivelComoIntent(t *testing.T) {
	led, _ := newLedger(t)

	if err := led.RecordIntent(name, at); err != nil {
		t.Fatalf("gravar intenção: %v", err)
	}

	entry, found, err := led.Lookup(name)
	if err != nil || !found {
		t.Fatalf("consultar: found=%v err=%v", found, err)
	}
	if entry.Estado != ledger.StateIntent {
		t.Errorf("estado = %q, esperava %q", entry.Estado, ledger.StateIntent)
	}
	if entry.Arquivo != name {
		t.Errorf("arquivo = %q, esperava %q", entry.Arquivo, name)
	}
	if entry.ConcluidoEm != "" {
		t.Errorf("intenção não pode ter conclusão; veio %q", entry.ConcluidoEm)
	}
}

// A segunda intenção sobre o mesmo nome é recusada. Se ela passasse, duas execuções concorrentes
// poderiam transmitir o mesmo arquivo — que é o desfecho que este pacote existe para impedir.
func TestSegundaIntencaoSobreOMesmoNomeEhRecusada(t *testing.T) {
	led, _ := newLedger(t)

	if err := led.RecordIntent(name, at); err != nil {
		t.Fatalf("primeira intenção: %v", err)
	}

	err := led.RecordIntent(name, at)
	if err == nil {
		t.Fatal("a segunda intenção deveria ser recusada")
	}
	if !errors.Is(err, ledger.ErrAlreadyRecorded) {
		t.Errorf("erro = %v, esperava ErrAlreadyRecorded", err)
	}
}

func TestConclusaoTransicionaIntentParaDone(t *testing.T) {
	led, _ := newLedger(t)

	if err := led.RecordIntent(name, at); err != nil {
		t.Fatalf("intenção: %v", err)
	}
	if err := led.RecordDone(name, "transmitido", at.Add(time.Minute)); err != nil {
		t.Fatalf("conclusão: %v", err)
	}

	entry, found, err := led.Lookup(name)
	if err != nil || !found {
		t.Fatalf("consultar: found=%v err=%v", found, err)
	}
	if entry.Estado != ledger.StateDone {
		t.Errorf("estado = %q, esperava %q", entry.Estado, ledger.StateDone)
	}
	if entry.Situacao != "transmitido" {
		t.Errorf("situação = %q", entry.Situacao)
	}
	// A intenção original permanece legível: é ela que diz QUANDO a tentativa começou, e quem
	// investiga um pagamento precisa da janela inteira.
	if entry.RegistradoEm == "" {
		t.Error("o carimbo da intenção foi perdido na conclusão")
	}
	if entry.ConcluidoEm == "" {
		t.Error("a conclusão precisa carimbar o desfecho")
	}
}

func TestConcluirSemIntencaoEhErro(t *testing.T) {
	led, _ := newLedger(t)

	if err := led.RecordDone(name, "transmitido", at); err == nil {
		t.Error("concluir sem intenção prévia deveria falhar")
	}
}

// Registro corrompido NÃO pode ser lido como ausente: ausente autoriza transmitir, e um registro
// ilegível é justamente o caso em que não se sabe se o arquivo já saiu.
func TestRegistroCorrompidoFalhaEmVezDeParecerAusente(t *testing.T) {
	led, dir := newLedger(t)

	if err := led.RecordIntent(name, at); err != nil {
		t.Fatalf("intenção: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("esperava 1 arquivo no registro; err=%v entries=%d", err, len(entries))
	}
	if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("{ não é json"), 0o640); err != nil {
		t.Fatalf("corromper: %v", err)
	}

	_, found, err := led.Lookup(name)
	if err == nil {
		t.Fatal("registro ilegível deveria produzir erro")
	}
	if found {
		t.Error("registro ilegível não pode ser reportado como encontrado")
	}
}

// O nome do arquivo de registro é derivado por hash, e não usado direto como caminho: o nome vem do
// bucket e não é confiável como componente de caminho.
func TestNomeHostilNaoEscapaDoDiretorioDoRegistro(t *testing.T) {
	led, dir := newLedger(t)

	hostil := "../../../etc/passwd"
	if err := led.RecordIntent(hostil, at); err != nil {
		t.Fatalf("gravar intenção: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listar registro: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("esperava 1 arquivo dentro do diretório do registro, veio %d", len(entries))
	}

	// E continua consultável pelo nome original, que fica dentro do JSON.
	entry, found, err := led.Lookup(hostil)
	if err != nil || !found {
		t.Fatalf("consultar: found=%v err=%v", found, err)
	}
	if entry.Arquivo != hostil {
		t.Errorf("arquivo = %q, esperava %q", entry.Arquivo, hostil)
	}
}

// Nomes diferentes não podem compartilhar registro — seria idempotência aplicada ao arquivo errado.
func TestNomesDiferentesTemRegistrosIndependentes(t *testing.T) {
	led, _ := newLedger(t)

	if err := led.RecordIntent("A.REM", at); err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := led.RecordIntent("B.REM", at); err != nil {
		t.Fatalf("B: %v", err)
	}
	if err := led.RecordDone("A.REM", "transmitido", at); err != nil {
		t.Fatalf("concluir A: %v", err)
	}

	b, found, err := led.Lookup("B.REM")
	if err != nil || !found {
		t.Fatalf("consultar B: found=%v err=%v", found, err)
	}
	if b.Estado != ledger.StateIntent {
		t.Errorf("B foi afetado pela conclusão de A: estado = %q", b.Estado)
	}
}
