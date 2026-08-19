package agent

// O ciclo de RECEPÇÃO — o caminho que não paga ninguém.
//
// É por ele que se começa a exercitar qualquer instalação real, e a razão é simples: transmitir
// errado tira dinheiro da conta de alguém, receber errado não. Enquanto não houver ambiente de
// homologação, este é o único ciclo que pode ser rodado contra a instalação de produção sem que um
// engano custe um pagamento.
//
// A ordem aqui é INVERSA à da transmissão, e a inversão é deliberada:
//
//	1. acionar o cliente          (modo R)
//	2. ler o log do ciclo         ← a evidência de origem
//	3. listar a pasta de ENTRADA
//	4. por arquivo: depositar no bucket, DEPOIS registrar
//	5. publicar o envelope
//	6. tirar o arquivo da pasta de entrada
//
// Na transmissão, registrar antes é o que impede pagar duas vezes. Aqui o risco é o oposto —
// PERDER evidência de um pagamento —, e registrar antes de depositar abriria exatamente essa
// janela: uma queda entre o registro e o depósito faria o ciclo seguinte reconhecer o conteúdo como
// já recebido e nunca depositá-lo. Depositar antes, no pior caso, deposita de novo o mesmo
// conteúdo sob a mesma chave, que é inócuo.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

// ReceiveOutcome é o que aconteceu com UM arquivo recebido.
type ReceiveOutcome struct {
	FileName string
	Sha256   string
	// StoredKey é onde o objeto foi depositado. Vazio quando nada foi depositado — duplicado ou
	// recusa de fronteira.
	StoredKey string
	// Correlated diz se o arquivo casou com linha de recepção do log deste ciclo.
	Correlated bool
	Duplicate  bool
	Situation  envelope.Situation
	StatusKey  string
	Archived   bool
	Err        error
}

// ReceiveSummary reúne o ciclo.
type ReceiveSummary struct {
	// ClientInvoked afirma que o cliente rodou. O ciclo de recepção SEMPRE o aciona: é ele que traz
	// os arquivos, e não acioná-lo tornaria o ciclo uma varredura de pasta.
	ClientInvoked bool
	ExitCode      *int
	Outcomes      []ReceiveOutcome
	// LoggedButAbsent são nomes que o log diz ter recebido e que não estavam na pasta.
	//
	// Não viram desfecho — o agente não inventa arquivo a partir do log —, mas precisam aparecer:
	// um arquivo que o cliente diz ter recebido e sumiu antes de alguém olhar é o desfecho que
	// ninguém percebe.
	LoggedButAbsent []string
}

// Errs devolve os erros acumulados. Um arquivo problemático não interrompe os demais.
func (s ReceiveSummary) Errs() []error {
	var out []error
	for _, o := range s.Outcomes {
		if o.Err != nil {
			out = append(out, o.Err)
		}
	}
	return out
}

// ReceiveCycle roda uma passada de recepção.
func (a *Agent) ReceiveCycle(ctx context.Context) (ReceiveSummary, error) {
	if a.reception == nil {
		return ReceiveSummary{}, errors.New("índice de recepção não configurado; o ciclo de recepção precisa dele")
	}

	var summary ReceiveSummary

	// PASSO 1 — acionar. Sem filtro: o `-f` restringe o que é ENVIADO (§6, p.14), e a recepção
	// traz o que o banco tiver para este perfil.
	exitCode, runErr := a.client.Run(ctx, stcp.ModeReceive, "")
	summary.ClientInvoked = true
	summary.ExitCode = exitCode

	// PASSO 2 — o log do ciclo, que é a evidência de origem.
	//
	// Falha ao ler o log NÃO aborta: o log é diagnóstico, e sem ele os arquivos continuam sendo
	// depositados, apenas sem correlação. Abortar aqui descartaria arquivos do banco por causa de um
	// arquivo de apoio.
	records := a.receptionRecords()

	// PASSO 3 — listar a pasta de entrada.
	names, err := a.sp.ListInbound()
	if err != nil {
		// Ao contrário do log, falha ao LISTAR aborta: sem a listagem não se sabe o que chegou, e
		// prosseguir seria reportar "nada a receber" sobre um mundo imaginário.
		return summary, fmt.Errorf("listar pasta de entrada: %w", err)
	}

	presente := map[string]bool{}
	for _, n := range names {
		presente[n] = true
	}
	for _, logged := range stcp.ReceivedFileNames(records) {
		if !presente[logged] {
			summary.LoggedButAbsent = append(summary.LoggedButAbsent, logged)
		}
	}

	summary.Outcomes = make([]ReceiveOutcome, 0, len(names))
	for _, name := range names {
		summary.Outcomes = append(summary.Outcomes, a.receiveOne(ctx, name, records, runErr))
	}
	return summary, nil
}

// receiveOne trata UM arquivo.
func (a *Agent) receiveOne(ctx context.Context, name string, records []stcp.Record, runErr error) ReceiveOutcome {
	out := ReceiveOutcome{FileName: name}

	// A guarda de fronteira vem ANTES de qualquer escrita, e antes até da leitura do conteúdo.
	//
	// A chave do envelope DERIVA do nome: um nome com separador ou travessia produziria escrita fora
	// de `status/`, e um nome carregando os marcadores reservados viraria, aos olhos do consumidor,
	// um status de outro tipo. Não há como publicar um desfecho para um nome assim sem mentir sobre
	// o que ele é — então nada é escrito, e o arquivo fica visível onde está.
	if err := envelope.ValidName(name); err != nil {
		out.Err = fmt.Errorf("arquivo recebido com nome inválido %q (nada foi escrito no bucket, "+
			"e o arquivo continua na pasta de entrada para conferência): %w", name, err)
		return out
	}

	content, err := a.sp.ReadInbound(name)
	if err != nil {
		out.Err = err
		return out
	}

	sum := sha256.Sum256(content)
	out.Sha256 = hex.EncodeToString(sum[:])

	linhas := stcp.RawLines(stcp.ReceptionLinesFor(records, name))
	out.Correlated = len(linhas) > 0

	entry, found, err := a.reception.Lookup(out.Sha256)
	if err != nil {
		// Sem saber o que o índice diz, NÃO se deposita e NÃO se arquiva: o arquivo fica na pasta e
		// a próxima passada tenta de novo. É o desfecho conservador — o conteúdo não se perde.
		out.Err = err
		return out
	}

	situation := envelope.Reception
	detail := "arquivo recebido do banco e depositado no prefixo de retorno"
	if !out.Correlated {
		// Erra-se para MAIS aqui, ao contrário da transmissão: descartar em silêncio um arquivo do
		// banco é o desfecho que ninguém percebe. Ele entra assim mesmo, marcado.
		detail = "arquivo encontrado na pasta de entrada SEM linha correspondente no log deste ciclo; " +
			"depositado assim mesmo, com a origem declarada como não correlacionada"
	}

	if found && entry.Estado == ledger.StateIntent {
		// CA5 — a mesma disciplina da transmissão: interrompido não vira sucesso presumido.
		//
		// O arquivo é depositado do mesmo jeito (nunca descartar), mas o desfecho publicado diz
		// REVISÃO: o ciclo anterior parou entre registrar e concluir, e ninguém sabe o que ficou
		// pela metade sem olhar.
		situation = envelope.Review
		detail = "o ciclo de recepção anterior foi interrompido depois de registrar a intenção e antes " +
			"de concluir; o arquivo foi depositado, e o desfecho exige conferência humana — nada aqui " +
			"é presumido como bem-sucedido"
	}

	if err := a.reception.RecordIntent(out.Sha256, name, a.cfg.Clock()); err != nil {
		out.Err = fmt.Errorf("registrar intenção de recepção de %q: %w", name, err)
		return out
	}

	key, err := a.storeReceived(ctx, name, content, out.Sha256)
	if err != nil {
		// A intenção fica gravada: a próxima passada verá `intent` e mandará para revisão, que é o
		// desfecho correto para um depósito de desfecho desconhecido.
		out.Err = err
		return out
	}
	out.StoredKey = key

	if err := a.reception.RecordDone(out.Sha256, key, a.cfg.Clock()); err != nil {
		out.Err = errors.Join(out.Err, fmt.Errorf("registrar recepção de %q: %w", name, err))
	}

	if runErr != nil {
		// O acionamento reclamou, mas os arquivos estão na pasta e o conteúdo está no bucket. A
		// evidência física prevalece, como na transmissão — o erro fica no detalhe para quem
		// investigar.
		detail += fmt.Sprintf("; o acionamento do cliente reportou erro (%v)", runErr)
	}

	out.Situation = situation
	out.StatusKey = envelope.ReceptionKey(name, a.cfg.Clock())
	a.publishReception(ctx, &out, detail, nil, linhas)

	// PASSO 6 — só agora o arquivo sai da pasta de entrada. Se isto falhar, ele fica lá e o ciclo
	// seguinte o encontra de novo: ruído visível, que é o desfecho certo comparado a perder a
	// evidência de um pagamento.
	if err := a.sp.Archive(name); err != nil {
		out.Err = errors.Join(out.Err, err)
	} else {
		out.Archived = true
	}
	return out
}

// storeReceived deposita o conteúdo no prefixo de retorno, sem NUNCA sobrescrever.
//
// Sobrescrever um arquivo de retorno destrói evidência de um pagamento — o pior lugar possível para
// perder registro. Quando a chave natural já está ocupada por conteúdo DIFERENTE, o objeto vai para
// uma chave desempatada pelo carimbo, e os dois permanecem recuperáveis.
//
// Quando a chave está ocupada pelo MESMO conteúdo, a escrita é dispensada: o objeto já é aquele. É
// o que torna a retomada de um ciclo interrompido inócua.
func (a *Agent) storeReceived(ctx context.Context, name string, content []byte, sum string) (string, error) {
	key := a.cfg.Prefixes.Returns + name

	existing, err := a.store.Get(ctx, key)
	switch {
	case err == nil:
		existingSum := sha256.Sum256(existing)
		if hex.EncodeToString(existingSum[:]) == sum {
			return key, nil
		}
		// Mesmo nome, conteúdo diferente: é arquivo NOVO, não duplicado. O nome é atribuído pelo
		// banco e não é identificador — tratá-lo como tal descartaria um retorno legítimo.
		key = fmt.Sprintf("%s%s.recebido-%s", a.cfg.Prefixes.Returns, name,
			a.cfg.Clock().UTC().Format(envelope.StampLayout))

	case errors.Is(err, bucket.ErrNotFound):
		// Caminho normal.

	default:
		return "", fmt.Errorf("conferir se %q já existe antes de depositar: %w", key, err)
	}

	if err := a.store.Put(ctx, key, content); err != nil {
		return "", fmt.Errorf("depositar arquivo recebido em %q: %w", key, err)
	}
	return key, nil
}

// publishReception publica o envelope de um arquivo recebido.
func (a *Agent) publishReception(
	ctx context.Context,
	out *ReceiveOutcome,
	detail string,
	exitCode *int,
	logLines []string,
) {
	env := envelope.NewReception(out.FileName, a.cfg.Clock(), out.Situation, detail, exitCode, logLines,
		envelope.ReceptionInfo{
			Sha256:         out.Sha256,
			Chave:          out.StoredKey,
			Correlacionado: out.Correlated,
			Duplicado:      out.Duplicate,
		})

	body, err := envelope.Marshal(env)
	if err != nil {
		out.Err = errors.Join(out.Err, err)
		return
	}
	if err := a.store.Put(ctx, out.StatusKey, body); err != nil {
		out.Err = errors.Join(out.Err, fmt.Errorf("publicar recepção em %q: %w", out.StatusKey, err))
	}
}

// receptionRecords lê e decodifica o log do ciclo.
//
// Devolve nada quando não há log: o log é DIAGNÓSTICO, e a ausência dele não pode impedir que
// arquivos do banco sejam depositados. O que se perde é a correlação, e isso o envelope declara.
func (a *Agent) receptionRecords() []stcp.Record {
	raw, err := a.sp.ReadTransferLog()
	if err != nil || raw == "" {
		return nil
	}
	return stcp.ParseLog(raw)
}
