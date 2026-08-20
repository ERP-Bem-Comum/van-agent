// Teste interno de propósito, como o `s3_internal_test.go` do pacote `bucket`: o que se afirma aqui
// é o comportamento de `verdict` e de `resumirErro`, que não são exportados — e exportá-los só para
// o teste alcançá-los mudaria a superfície do pacote por uma razão que não é do pacote.
package agent

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
)

// spoolQueNaoConsegueLer encena a instalação em que a evidência física está ilegível — permissão
// negada, pasta remota fora do ar, arquivo em uso. É o caminho do desfecho AMBÍGUO, e o pior caso
// medido do `detalhe`: ele interpola DOIS erros, cada um carregando o caminho completo.
type spoolQueNaoConsegueLer struct{ err error }

func (s spoolQueNaoConsegueLer) InOutbound(string) (bool, error) { return false, s.err }
func (s spoolQueNaoConsegueLer) InBackup(string) (bool, error)   { return false, s.err }

func (spoolQueNaoConsegueLer) Place(string, []byte) error             { return nil }
func (spoolQueNaoConsegueLer) ReadTransferLog() (string, bool, error) { return "", false, nil }
func (spoolQueNaoConsegueLer) ListInbound() ([]string, error)         { return nil, nil }
func (spoolQueNaoConsegueLer) ReadInbound(string) ([]byte, error)     { return nil, nil }
func (spoolQueNaoConsegueLer) Archive(string) error                   { return nil }

// erroDeCaminhoLongo reproduz a forma real do erro que o spool devolve — `consultar %q: %w`, com o
// caminho completo — num caminho fundo, do tipo que Windows permite até 260 caracteres.
func erroDeCaminhoLongo() error {
	caminho := `C:\` + strings.Repeat(`pasta-de-nome-longo\`, 12) + `PAG_000000.REM`
	return errors.New("consultar \"" + caminho + "\": " +
		"O processo não pode acessar o arquivo porque ele está sendo usado por outro processo.")
}

func TestCA3_TextoFixoDoDetalheSobreviveAoCorteDoErro(t *testing.T) {
	a := &Agent{sp: spoolQueNaoConsegueLer{err: erroDeCaminhoLongo()}}

	situacao, detalhe := a.verdict("PAG_000000.REM", nil, nil)

	// O desfecho não muda: evidência ilegível continua sendo revisão, nunca falha.
	if situacao != envelope.Review {
		t.Errorf("evidência ilegível precisa virar %q; veio %q", envelope.Review, situacao)
	}

	// O que o corte NÃO pode levar: a frase que diz ao operador o que aconteceu.
	for _, exigido := range []string{
		"não foi possível conferir a evidência física",
		"desfecho desconhecido",
		`"PAG_000000.REM"`,
	} {
		if !strings.Contains(detalhe, exigido) {
			t.Errorf("o corte comeu texto fixo — falta %q em:\n%s", exigido, detalhe)
		}
	}

	// E o resultado cabe no teto do contrato SEM que o piso do envelope precise cortar de novo:
	// se este número passar de MaxDetailLength, o orçamento por erro está grande demais.
	if n := utf8.RuneCountInString(detalhe); n > envelope.MaxDetailLength {
		t.Errorf("detalhe com %d caracteres antes do piso do contrato (%d) — o orçamento por erro "+
			"não está segurando o pior caso", n, envelope.MaxDetailLength)
	}

	// A cauda de cada erro foi cortada, e o corte está marcado nos DOIS.
	if c := strings.Count(detalhe, marcaDeCorte); c != 2 {
		t.Errorf("esperava marca de corte nos dois erros interpolados; encontrei %d em:\n%s", c, detalhe)
	}
}

func TestResumirErroPreservaOQueCabeECortaOQueNaoCabe(t *testing.T) {
	curto := errors.New("acesso negado")
	if got := resumirErro(curto); got != "acesso negado" {
		t.Errorf("erro que cabe foi alterado: %q", got)
	}

	longo := resumirErro(erroDeCaminhoLongo())
	if n := utf8.RuneCountInString(longo); n != maxErroNoDetalhe {
		t.Errorf("erro cortado precisa ocupar exatamente o orçamento %d; ocupou %d",
			maxErroNoDetalhe, n)
	}
	if !strings.HasSuffix(longo, marcaDeCorte) {
		t.Errorf("erro cortado sem marca: %q", longo)
	}
	if !strings.HasPrefix(longo, "consultar ") {
		t.Errorf("o corte precisa preservar o começo do erro, que é o que diz o que se tentou: %q", longo)
	}
}

func TestResumirErroNaoApagaOLadoQueNaoFalhou(t *testing.T) {
	// A frase do desfecho ambíguo nomeia os DOIS lados (saída e backup). Quando só um falha, o
	// outro precisa continuar aparecendo como `<nil>` — que é o que o `%v` fazia. Trocar por texto
	// vazio esconderia QUAL dos dois ficou ilegível, que é a única informação acionável ali.
	if got := resumirErro(nil); got != "<nil>" {
		t.Errorf("erro nil precisa continuar visível como \"<nil>\"; veio %q", got)
	}
}

func TestCorteDoErroRespeitaUTF8(t *testing.T) {
	// A mensagem do Windows em PT-BR é acentuada. Cortar por byte partiria um caractere ao meio.
	acentuado := errors.New(strings.Repeat("configuração inválida ", 40))
	got := resumirErro(acentuado)
	if !utf8.ValidString(got) {
		t.Fatal("o corte partiu um caractere ao meio")
	}
}
