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
	"time"

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
	// LogDoCicloLido diz se o log desta execução foi lido. Sem ele, `Correlated` false é ambíguo.
	LogDoCicloLido bool
	Duplicate      bool
	Situation      envelope.Situation
	StatusKey      string
	Archived       bool
	Err            error
}

// ReceiveSummary reúne o ciclo.
type ReceiveSummary struct {
	// ClientInvoked afirma que o cliente rodou. O ciclo de recepção SEMPRE o aciona: é ele que traz
	// os arquivos, e não acioná-lo tornaria o ciclo uma varredura de pasta.
	ClientInvoked bool
	ExitCode      *int
	// LogDoCicloLido afirma que o log foi lido E traz linha desta execução. `false` é "não sei o que
	// o cliente registrou", nunca "o cliente não registrou nada" — a distinção é o que impede o
	// consumidor de represar pagamento com base numa ignorância disfarçada de conclusão.
	LogDoCicloLido bool
	// Republicados conta envelopes de ciclos anteriores cuja publicação só se confirmou agora.
	// Diferente de zero significa que houve órfão no bucket até esta passada.
	Republicados int
	// ReconcileErr é o que falhou ao republicar. Fica separado dos erros dos arquivos porque não é
	// sobre nenhum arquivo deste ciclo — é sobre um desfecho antigo que continua sem sair.
	ReconcileErr error
	Outcomes     []ReceiveOutcome
	// LoggedButAbsent são nomes que o log diz ter recebido e que não estavam na pasta.
	//
	// Não viram desfecho — o agente não inventa arquivo a partir do log —, mas precisam aparecer:
	// um arquivo que o cliente diz ter recebido e sumiu antes de alguém olhar é o desfecho que
	// ninguém percebe.
	LoggedButAbsent []string
}

// Errs devolve os erros acumulados. Um arquivo problemático não interrompe os demais.
//
// A falha de reconciliação entra aqui de propósito: ela precisa fazer o ciclo sair com erro, senão
// um desfecho que nunca chegou ao bucket ficaria pendente sem nada chamar atenção — que é o
// desfecho que este mecanismo existe para eliminar.
func (s ReceiveSummary) Errs() []error {
	var out []error
	if s.ReconcileErr != nil {
		out = append(out, s.ReconcileErr)
	}
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

	// PASSO 0 — desfechos que já aconteceram e não chegaram a ser publicados têm precedência sobre
	// trabalho novo: enquanto não saem, existe objeto no bucket que ninguém consegue interpretar.
	// Não aborta o ciclo — o que falhou em publicar não pode impedir o que ainda vai chegar.
	republicados, err := a.ReconcilePending(ctx)
	summary.Republicados = republicados
	summary.ReconcileErr = err

	// O início da janela é marcado ANTES de acionar o cliente, e a ordem é o que dá sentido à
	// janela: só assim toda linha que o cliente escrever nesta execução cai depois dela. Marcar
	// depois deixaria de fora exatamente as linhas que se quer reconhecer.
	inicioDoCiclo := a.cfg.Clock()

	// PASSO 1 — acionar. Sem filtro: o `-f` restringe o que é ENVIADO (§6, p.14), e a recepção
	// traz o que o banco tiver para este perfil.
	exitCode, runErr := a.client.Run(ctx, stcp.ModeReceive, "")
	summary.ClientInvoked = true
	summary.ExitCode = exitCode

	// PASSO 2 — o log DESTE ciclo, que é a evidência de origem.
	//
	// Falha ao ler o log NÃO aborta: o log é diagnóstico, e sem ele os arquivos continuam sendo
	// depositados, apenas sem correlação. Abortar aqui descartaria arquivos do banco por causa de um
	// arquivo de apoio.
	records, logDoCicloLido := a.receptionRecords(inicioDoCiclo, a.cfg.Clock())
	summary.LogDoCicloLido = logDoCicloLido

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
		summary.Outcomes = append(summary.Outcomes, a.receiveOne(ctx, name, records, logDoCicloLido, runErr))
	}
	return summary, nil
}

// receiveOne trata UM arquivo.
func (a *Agent) receiveOne(
	ctx context.Context,
	name string,
	records []stcp.Record,
	logDoCicloLido bool,
	runErr error,
) ReceiveOutcome {
	out := ReceiveOutcome{FileName: name, LogDoCicloLido: logDoCicloLido}

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
		//
		// As duas frases são distintas porque as ações são distintas, e quem as lê é um operador às
		// três da manhã: uma manda conferir o ARQUIVO, a outra manda conferir a INSTALAÇÃO. A versão
		// anterior dizia "log deste ciclo" nos dois casos — afirmação que o código não sustentava
		// quando o log deste ciclo sequer havia sido lido.
		if logDoCicloLido {
			detail = "arquivo encontrado na pasta de entrada SEM linha correspondente no log deste " +
				"ciclo, que FOI lido; depositado assim mesmo, com a origem declarada como não " +
				"correlacionada — o cliente não registrou tê-lo recebido nesta execução"
		} else {
			detail = "arquivo encontrado na pasta de entrada e depositado, mas o log DESTA execução " +
				"não pôde ser lido (padrão sem correspondência, log ainda não escrito ou leitura que " +
				"falhou); a ausência de correlação NÃO é indício sobre o arquivo — é sobre a " +
				"configuração do log na instalação, e é ela que precisa ser conferida"
		}
	}

	if found && entry.Estado == ledger.StateDone {
		// CA1/CA3 — este CONTEÚDO já foi recebido antes.
		//
		// Reconhecido pelo HASH, nunca pelo nome: o nome é atribuído pelo banco, o mesmo arquivo
		// pode voltar com nome diferente, e nomes iguais podem trazer conteúdo diferente. Deduplicar
		// por nome produziria as duas falhas opostas — descartar arquivo novo e aceitar reenvio como
		// novidade.
		//
		// O objeto anterior NÃO é tocado. Sobrescrever um arquivo de retorno destrói evidência de um
		// pagamento, que é o pior lugar possível para perder registro; e regravar o mesmo conteúdo
		// sob outra chave só produziria uma cópia idêntica que ninguém sabe ligar à original.
		return a.receiveDuplicate(ctx, out, entry, linhas, name)
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
			LogDoCicloLido: out.LogDoCicloLido,
			Duplicado:      out.Duplicate,
		})

	body, err := envelope.Marshal(env)
	if err != nil {
		out.Err = errors.Join(out.Err, err)
		return
	}
	// Passa pelo registro de pendências porque o `Archive` que vem logo depois tira o arquivo da
	// pasta de ENTRADA: sem retomada, uma falha aqui deixaria o objeto em `retorno/` sem envelope, e
	// o ciclo seguinte nem veria o arquivo para tentar de novo.
	if err := a.publishEnvelope(ctx, out.StatusKey, out.FileName, body); err != nil {
		out.Err = errors.Join(out.Err, err)
	}
}

// clockSkewMargin alarga a janela do ciclo nas duas pontas.
//
// O carimbo do §12 tem resolução de SEGUNDO, e o agente marca o início do ciclo com resolução de
// nanossegundo. Sem margem, uma linha escrita 300ms depois do início pode trazer o carimbo do
// segundo anterior e cair fora da janela — rejeitando como "de outro ciclo" uma linha que é deste.
// Um segundo cobre o truncamento; ampliar mais reabriria a porta para o log de ontem, que é o que a
// janela existe para fechar.
const clockSkewMargin = time.Second

// receptionRecords lê o log e devolve SÓ as linhas deste ciclo, mais a afirmação de que o log do
// ciclo foi efetivamente lido.
//
// Devolve nada quando não há log: o log é DIAGNÓSTICO, e a ausência dele não pode impedir que
// arquivos do banco sejam depositados. O que se perde é a correlação, e isso o envelope declara.
//
// O segundo retorno é a diferença entre "não sei" e "sei que não tinha", e ele só é `true` quando as
// DUAS condições valem: o arquivo de log foi lido, e ele contém pelo menos uma linha carimbada
// dentro desta janela. A segunda condição é o que impede o modo de falha diário — o nome do log
// começa por data (§7, p.15), então no primeiro ciclo do dia o padrão casa o log de ONTEM, e uma
// leitura bem-sucedida de um log real não diz nada sobre o que acabou de acontecer.
func (a *Agent) receptionRecords(from, to time.Time) ([]stcp.Record, bool) {
	raw, read, err := a.sp.ReadTransferLog()
	if err != nil || !read {
		return nil, false
	}

	doCiclo := stcp.WithinWindow(stcp.ParseLog(raw), from.Add(-clockSkewMargin), to.Add(clockSkewMargin))
	// Log lido e sem NENHUMA linha deste ciclo é tratado como "não sei", e não como "sei que não
	// tinha". É o caso do log de ontem, e também o do arquivo vazio: nos dois, o que o cliente
	// registrou nesta execução continua desconhecido. Afirmar o contrário daria à ausência de
	// correlação uma confiança que nada sustenta — e é sobre essa confiança que o consumidor decide
	// represar pagamento.
	return doCiclo, len(doCiclo) > 0
}

// receiveDuplicate trata a reaparição de um conteúdo já recebido.
//
// Publica, e não silencia, porque o consumidor precisa saber que o banco reenviou algo: a ausência
// de envelope já significa "rodou e não havia nada a receber", e usar o mesmo silêncio para duas
// situações diferentes apagaria a única pista de que houve reenvio.
//
// A chave do envelope é distinta da anterior — o carimbo e o nome a compõem —, então o envelope da
// primeira recepção continua legível. É a mesma forma que a transmissão já usa para a tentativa
// duplicada, e o consumidor já sabe classificá-la.
func (a *Agent) receiveDuplicate(
	ctx context.Context,
	out ReceiveOutcome,
	entry ledger.ReceptionEntry,
	logLines []string,
	name string,
) ReceiveOutcome {
	out.Duplicate = true
	out.Situation = envelope.Reception
	out.StatusKey = envelope.ReceptionKey(name, a.cfg.Clock())

	detail := fmt.Sprintf(
		"conteúdo idêntico a uma recepção anterior (mesmo sha256), já depositado em %q em %s; "+
			"o objeto anterior NÃO foi sobrescrito e nada foi depositado de novo",
		entry.Chave, entry.ConcluidoEm)
	if entry.Arquivo != name {
		// O caso que só o hash pega: mesmo arquivo, nome diferente. Registrar os dois nomes é o que
		// permite a quem investiga entender por que um arquivo "novo" não gerou objeto novo.
		detail = fmt.Sprintf(
			"conteúdo idêntico a uma recepção anterior (mesmo sha256), recebida com o nome %q e já "+
				"depositada em %q em %s; o nome mudou, o conteúdo não — o objeto anterior NÃO foi "+
				"sobrescrito e nada foi depositado de novo",
			entry.Arquivo, entry.Chave, entry.ConcluidoEm)
	}

	env := envelope.NewReception(name, a.cfg.Clock(), out.Situation, detail, nil, logLines,
		envelope.ReceptionInfo{
			Sha256:         out.Sha256,
			Chave:          entry.Chave,
			Correlacionado: out.Correlated,
			LogDoCicloLido: out.LogDoCicloLido,
			Duplicado:      true,
			DuplicadoDe:    entry.Chave,
		})

	body, err := envelope.Marshal(env)
	if err != nil {
		out.Err = errors.Join(out.Err, err)
		return out
	}
	if err := a.publishEnvelope(ctx, out.StatusKey, name, body); err != nil {
		out.Err = errors.Join(out.Err, err)
		return out
	}

	// Arquiva do mesmo jeito: deixar na entrada faria o duplicado ser republicado a cada passada, e
	// o ruído esconderia o caso real.
	if err := a.sp.Archive(name); err != nil {
		out.Err = errors.Join(out.Err, err)
	} else {
		out.Archived = true
	}
	return out
}
