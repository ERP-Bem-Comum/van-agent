package stcp_test

import (
	"strings"
	"testing"

	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

// buildLine monta uma linha campo a campo, com as dez larguras do §12 (p.30).
//
// O teste monta a linha a partir das LARGURAS e o parser a lê a partir dos OFFSETS. São dois
// caminhos independentes para a mesma tabela: se alguém errar um offset no parser, esta montagem
// não acompanha o erro, e o teste quebra. Um teste que reusasse os offsets do parser não provaria
// nada — concordaria com o defeito.
func buildLine(occurredAt, op, profile, process, procID, threadID, result, size, fileName, info string) string {
	pad := func(s string, w int) string {
		if len(s) > w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	padNum := func(s string, w int) string {
		if len(s) > w {
			return s[len(s)-w:]
		}
		return strings.Repeat("0", w-len(s)) + s
	}
	return padNum(occurredAt, 14) + padNum(op, 4) + pad(profile, 30) + pad(process, 16) +
		pad(procID, 8) + pad(threadID, 8) + padNum(result, 6) + padNum(size, 12) +
		pad(fileName, 256) + pad(info, 128)
}

func TestParseLineDecodificaOsDezCampos(t *testing.T) {
	line := buildLine(
		"20260818120000", "0005", "PERFIL", "STCPCLT", "0000ABCD", "0000EF01",
		"000000", "240", "PAG_000000.20260818120000_0001.REM", "transferencia concluida",
	)

	if len(line) != stcp.LineWidth {
		t.Fatalf("a linha montada tem %d caracteres, o layout declara %d", len(line), stcp.LineWidth)
	}

	got := stcp.ParseLine(line)

	casos := []struct {
		campo string
		got   string
		want  string
	}{
		{"1 · data/hora", got.OccurredAt, "20260818120000"},
		{"2 · operação", got.Op, "0005"},
		{"3 · perfil", got.Profile, "PERFIL"},
		{"4 · processo", got.Process, "STCPCLT"},
		{"5 · cód. processo", got.ProcessID, "0000ABCD"},
		{"6 · cód. thread", got.ThreadID, "0000EF01"},
		{"7 · resultado", got.Result, "000000"},
		{"8 · tamanho", got.Size, "000000000240"},
		{"9 · arquivo", got.FileName, "PAG_000000.20260818120000_0001.REM"},
		{"10 · informações", got.Info, "transferencia concluida"},
	}
	for _, c := range casos {
		if c.got != c.want {
			t.Errorf("campo %s = %q, esperava %q", c.campo, c.got, c.want)
		}
	}
}

func TestSucessoEhSomenteOCodigoZerado(t *testing.T) {
	ok := stcp.ParseLine(buildLine("20260818120000", "0005", "P", "S", "1", "1", "000000", "1", "A.REM", ""))
	if !ok.Succeeded() {
		t.Error("resultado 000000 deveria ser sucesso")
	}
	// 000401 — §11, p.24.
	nok := stcp.ParseLine(buildLine("20260818120000", "0005", "P", "S", "1", "1", "000401", "1", "A.REM", ""))
	if nok.Succeeded() {
		t.Error("resultado 000401 não pode contar como sucesso")
	}
}

// A tolerância existe porque o parser roda DEPOIS de uma transmissão que já aconteceu: abortar
// aqui trocaria diagnóstico incompleto por desfecho desconhecido.
func TestLinhaCurtaNaoDerrubaOParser(t *testing.T) {
	got := stcp.ParseLine("2026081812000000051")

	if got.OccurredAt != "20260818120000" {
		t.Errorf("data/hora = %q", got.OccurredAt)
	}
	if got.Op != "0005" {
		t.Errorf("operação = %q", got.Op)
	}
	if got.FileName != "" {
		t.Errorf("campos além do fim da linha devem sair vazios; arquivo = %q", got.FileName)
	}
}

func TestParseLogAceitaCRLFEDescartaLinhaVazia(t *testing.T) {
	a := buildLine("20260818120000", "0004", "P", "S", "1", "1", "000000", "1", "A.REM", "")
	b := buildLine("20260818120001", "0005", "P", "S", "1", "1", "000000", "1", "A.REM", "")

	got := stcp.ParseLog(a + "\r\n" + b + "\r\n\r\n")

	if len(got) != 2 {
		t.Fatalf("esperava 2 registros, veio %d", len(got))
	}
	if got[0].Op != stcp.OpSendStart || got[1].Op != stcp.OpSendEnd {
		t.Errorf("operações = %q, %q", got[0].Op, got[1].Op)
	}
}

// A correlação é por nome EXATO. Prefixo atribuiria a linha de um arquivo a outro — e o desfecho
// disso é dar por transmitida uma remessa que não saiu.
func TestFilterByFileNaoCasaPorPrefixo(t *testing.T) {
	nosso := buildLine("20260818120000", "0005", "P", "S", "1", "1", "000000", "1", "PAG_1.REM", "")
	vizinho := buildLine("20260818120001", "0005", "P", "S", "1", "1", "000000", "1", "PAG_10.REM", "")

	got := stcp.FilterByFile(stcp.ParseLog(nosso+"\r\n"+vizinho), "PAG_1.REM")

	if len(got) != 1 {
		t.Fatalf("esperava 1 registro, veio %d — o filtro casou por prefixo", len(got))
	}
	if got[0].FileName != "PAG_1.REM" {
		t.Errorf("arquivo = %q", got[0].FileName)
	}
}

func TestSendOutcomeResumeInicioEFim(t *testing.T) {
	start := buildLine("20260818120000", "0004", "P", "S", "1", "1", "000000", "1", "A.REM", "")
	end := buildLine("20260818120001", "0005", "P", "S", "1", "1", "000000", "1", "A.REM", "")

	got := stcp.SendOutcomeFor(stcp.ParseLog(start+"\r\n"+end), "A.REM")

	if !got.Started || !got.Finished || !got.Succeeded {
		t.Errorf("resumo = %+v, esperava início, fim e sucesso", got)
	}
	if got.FailureCode != "" {
		t.Errorf("código de falha = %q, esperava vazio", got.FailureCode)
	}
}

func TestSendOutcomeGuardaOCodigoDaFalha(t *testing.T) {
	start := buildLine("20260818120000", "0004", "P", "S", "1", "1", "000000", "1", "A.REM", "")
	end := buildLine("20260818120001", "0005", "P", "S", "1", "1", "000503", "1", "A.REM", "")

	got := stcp.SendOutcomeFor(stcp.ParseLog(start+"\r\n"+end), "A.REM")

	if got.Succeeded {
		t.Error("não pode contar como sucesso")
	}
	// 000503 — §11, p.25: OdetteID não cadastrado. É erro de IDENTIDADE, e o CA6 exige que ele não
	// vire retentativa cega. Aqui o que se cobra é que o código CHEGUE a quem decide.
	if got.FailureCode != "000503" {
		t.Errorf("código de falha = %q, esperava 000503", got.FailureCode)
	}
}

// O cliente é configurado com retentativas (`-r`, §6 p.14): uma tentativa que falha e outra que
// funciona produzem duas linhas de fim. A última é a que vale.
func TestUltimaTentativaBemSucedidaApagaOCodigoDeFalhaAnterior(t *testing.T) {
	falha := buildLine("20260818120000", "0005", "P", "S", "1", "1", "010048", "1", "A.REM", "")
	sucesso := buildLine("20260818120005", "0005", "P", "S", "1", "1", "000000", "1", "A.REM", "")

	got := stcp.SendOutcomeFor(stcp.ParseLog(falha+"\r\n"+sucesso), "A.REM")

	if !got.Succeeded {
		t.Error("a última tentativa foi bem-sucedida")
	}
	if got.FailureCode != "" {
		t.Errorf("código de falha = %q, esperava vazio após sucesso", got.FailureCode)
	}
}
