// Teste interno de propósito, como o `s3_internal_test.go` do pacote `bucket`: o que se afirma aqui
// é o comportamento de `verdict` e de `resumirErro`, que não são exportados — e exportá-los só para
// o teste alcançá-los mudaria a superfície do pacote por uma razão que não é do pacote.
package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
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

// ⚠️ TRAVA DE COMPILAÇÃO — `verdict` não pode enxergar o log.
//
// O veredito vem da evidência física (ADR-0061 §2); o log é diagnóstico. A garantia disso não é uma
// asserção de teste, é a ASSINATURA: `verdict` recebe o nome, o código de saída e o erro de
// execução, e mais nada. Se alguém lhe passar as linhas do log ou o `stcp.SendOutcome` — para
// "melhorar" o desfecho —, este arquivo deixa de compilar, e o defeito aparece na hora em vez de
// aparecer num "sucesso" no log com o arquivo parado na fila do banco.
//
// Um teste comum não pegaria isso: ele afirmaria o comportamento de hoje, e o comportamento novo
// seria escrito junto com o teste novo. A assinatura é o que não se altera sem intenção.
var _ func(*Agent, string, *int, error) (envelope.Situation, string) = (*Agent).verdict

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

// A pergunta do core-api, virada em teste: quando o `detalhe` é truncado, o código do §11 sobrevive
// ou é a primeira coisa a cair?
//
// A suspeita dele tinha fundamento estrutural: o corte do contrato preserva o COMEÇO, e o código
// entra no FIM. Se o desfecho for o do caminho mais longo — evidência física ilegível, que interpola
// dois erros —, o campo que a (c) existe para entregar seria justamente o que o teto remove. E do
// lado dele, `detalhe` truncado SEM código é indistinguível de "não houve código".
func TestCodigoDoBancoSobreviveAoTetoDoContrato(t *testing.T) {
	// Antes da reserva, isto degradava com o COMPRIMENTO DO NOME, e sem nenhum aviso: com 34
	// caracteres (o nome que a própria suíte usa) a referência ao §11 já saía mutilada; com 40 ela
	// sumia; com 60 sumia o código. Por isso o teste varre faixas em vez de fixar um nome — um caso
	// só teria passado por acidente de ordem das palavras e declarado a garantia como existente.
	//
	// O nome NÃO é nosso: ele vem do core-api byte a byte, e a trava de comprimento
	// (`VAN_AGENT_NAME_MAX_LENGTH`) é opt-in por ambiente. Supor que ele é curto é supor
	// configuração alheia.
	a := &Agent{sp: spoolQueNaoConsegueLer{err: erroDeCaminhoLongo()}}

	for _, n := range []int{20, 34, 40, 60, 80, 120} {
		nome := strings.Repeat("N", n-4) + ".REM"

		_, detalhe := a.verdict(nome, nil, nil)
		detalhe = comCodigoDoBanco(detalhe, stcp.SendOutcome{Finished: true, FailureCode: "000401"})

		// Passa pelo piso do contrato: é o que o consumidor recebe de fato.
		env := envelope.New(nome, time.Unix(0, 0), envelope.Review, detalhe, nil, nil)

		if n := utf8.RuneCountInString(env.Detalhe); n > envelope.MaxDetailLength {
			t.Errorf("nome de %d: detalhe entregue com %d caracteres", len(nome), n)
		}
		// 1º — o código, que é o que esta função existe para entregar. Do lado do consumidor, a
		// ausência dele é indistinguível de "não houve código", que é a conclusão oposta.
		if !strings.Contains(env.Detalhe, "000401") {
			t.Errorf("nome de %d caracteres: o código do banco NÃO sobreviveu ao teto.\n%s",
				len(nome), env.Detalhe)
		}
		// 2º — a referência que diz onde procurar o significado do código.
		if !strings.Contains(env.Detalhe, "§11, pp. 24-29)") {
			t.Errorf("nome de %d caracteres: a referência ao §11 saiu mutilada — um código sem a\n"+
				"tabela obriga quem lê a adivinhar onde procurar.\n%s", len(nome), env.Detalhe)
		}
		// 3º — a instrução ao operador, que o código não pode ter comido para sobreviver.
		if !strings.Contains(env.Detalhe, "não foi possível conferir a evidência física") {
			t.Errorf("nome de %d caracteres: o corte comeu a instrução ao operador.\n%s",
				len(nome), env.Detalhe)
		}
		// E o corte, quando acontece, continua declarado.
		if utf8.RuneCountInString(detalhe) > envelope.MaxDetailLength &&
			!strings.Contains(env.Detalhe, marcaDeCorte) {
			t.Errorf("nome de %d caracteres: houve corte e ele não foi marcado.\n%s",
				len(nome), env.Detalhe)
		}
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
