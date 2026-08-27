package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ERP-Bem-Comum/van-agent/internal/agent"
)

// O relatório do console é o que o operador lê, e até aqui ele não tinha teste nenhum: este pacote
// não tinha arquivo de teste.
//
// A lacuna importa porque a lógica de severidade mora AQUI, não no `agent`. A suíte do `agent` cobre
// quais combinações de `LogDoCicloLido`/`LogEncontrado` o ciclo produz; o que ela não cobre é o que
// se IMPRIME para cada uma — que é justamente onde o defeito estava.
//
// ⚠️ E o ramo do silêncio só é verificável assim. Uma inspeção de console não distingue "a função
// decidiu não escrever" de "a função não rodou", então a verificação em campo — que confirmou os
// outros dois ramos numa instalação de apoio — não alcança este. Um teste alcança.

// O que se afirma aqui não é o texto, é a SEVERIDADE: o `⚠️` é o que o operador enxerga antes de
// ler, e é a diferença entre "há algo a consertar" e "isto é rotina".
func TestRelatorioDoLogSeparaOsTresEstadosPorSeveridade(t *testing.T) {
	const glob = "*.LOG"
	const dir = "C:\\STCP\\LOG"

	casos := []struct {
		nome           string
		summary        agent.ReceiveSummary
		esperaAlarme   bool
		esperaSilencio bool
		// contem são trechos que precisam aparecer. Vazio quando o esperado é silêncio.
		contem []string
		// naoContem guarda o que a mensagem NÃO pode afirmar. É a metade que pega a regressão: o
		// defeito original não era texto ausente, era texto a MAIS — uma causa nomeada com confiança
		// onde ela não se aplicava.
		naoContem []string
	}{
		{
			nome:         "nenhum log foi lido — é defeito, e o único que manda conferir o padrão",
			summary:      agent.ReceiveSummary{LogDoCicloLido: false, LogEncontrado: false},
			esperaAlarme: true,
			// O padrão e a pasta em uso vão na mensagem porque quem lê está na máquina do cliente
			// STCP, sem o `.env` à mão — e foi assim que a causa foi encontrada em campo.
			//
			// A pasta é conferida na forma LITERAL, com uma barra só. É o que pega a regressão que
			// este caso já encontrou: formatada com `%q`, ela saía como `C:\\STCP\\LOG`, e um caminho
			// com as barras dobradas faz o operador duvidar do valor em vez de conferi-lo.
			contem:    []string{"*.LOG", `C:\STCP\LOG`, "VAN_AGENT_STCP_TRANSFER_LOG_GLOB"},
			naoContem: []string{`C:\\STCP\\LOG`},
		},
		{
			nome:         "log lido, sem linha desta execução — rotina, e a ressalva continua sendo dita",
			summary:      agent.ReceiveSummary{LogDoCicloLido: false, LogEncontrado: true},
			esperaAlarme: false,
			contem: []string{
				"log lido",
				// Sem isto o operador fica com o fato e sem a explicação, e supõe defeito.
				"normal em ciclo sem transferência",
				// A ressalva é sobre o que o agente NÃO sabe, e isso não mudou com a leitura do log.
				"diz respeito ao arquivo",
			},
			// A REGRESSÃO que este caso existe para impedir: era exatamente aqui que o aviso antigo
			// mandava conferir o padrão do log — a única das duas causas que não se aplica quando o
			// log FOI lido. Medido em campo: com o padrão corrigido, o aviso continuava acusando-o.
			naoContem: []string{"VAN_AGENT_STCP_TRANSFER_LOG_GLOB", "conferir"},
		},
		{
			nome:           "log lido com linha desta execução — silêncio",
			summary:        agent.ReceiveSummary{LogDoCicloLido: true, LogEncontrado: true},
			esperaSilencio: true,
		},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			var out bytes.Buffer
			relatarLeituraDoLog(&out, c.summary, glob, dir)
			saida := out.String()

			if c.esperaSilencio {
				if saida != "" {
					t.Fatalf("o caso saudável precisa sair calado; veio: %q", saida)
				}
				return
			}
			if saida == "" {
				t.Fatal("saiu calado, e este estado precisa ser dito")
			}

			// `⚠️` é a marca de severidade, e só o primeiro estado a merece. Usá-la no ciclo ocioso —
			// que é o caso COMUM num agente one-shot que roda muitas vezes por dia e transmite poucas —
			// deixaria o alarme ligado quase sempre, e alarme ligado quase sempre treina quem lê a
			// ignorá-lo.
			if temAlarme := strings.Contains(saida, "⚠️"); temAlarme != c.esperaAlarme {
				t.Errorf("alarme = %v, esperava %v; saída: %q", temAlarme, c.esperaAlarme, saida)
			}
			for _, trecho := range c.contem {
				if !strings.Contains(saida, trecho) {
					t.Errorf("a saída precisa conter %q; veio: %q", trecho, saida)
				}
			}
			for _, trecho := range c.naoContem {
				if strings.Contains(saida, trecho) {
					t.Errorf("a saída NÃO pode conter %q neste estado; veio: %q", trecho, saida)
				}
			}
		})
	}
}

// O estado impossível não pode passar por saudável.
//
// `LogDoCicloLido` é a conjunção, então `true` com `LogEncontrado: false` significa "há linha desta
// janela num log que não foi lido" — contradição. Se um refactor inverter as condições em
// `receptionRecords`, é assim que ela chega aqui, e o pior desfecho seria o ramo do SILÊNCIO: o
// relatório passaria a calar sobre um log que nunca foi lido, que é a regressão mais cara possível
// deste conserto, porque some sem deixar rastro.
func TestRelatorioNaoTrataEstadoContraditorioComoSaudavel(t *testing.T) {
	var out bytes.Buffer
	relatarLeituraDoLog(&out, agent.ReceiveSummary{LogDoCicloLido: true, LogEncontrado: false}, "*.LOG", "/log")

	if saida := out.String(); saida != "" {
		// Documenta a escolha em vez de deixá-la implícita: a conjunção manda, e o ramo do silêncio é
		// o dela. O que este teste garante é que a escolha é DELIBERADA e visível se alguém mudá-la.
		t.Fatalf("hoje a conjunção manda e o resultado é silêncio; se isto mudou, foi decisão de "+
			"alguém e precisa estar escrita — veio: %q", saida)
	}
}
