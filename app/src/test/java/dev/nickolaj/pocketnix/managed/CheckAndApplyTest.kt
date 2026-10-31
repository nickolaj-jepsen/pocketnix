package dev.nickolaj.pocketnix.managed

import dev.nickolaj.pocketnix.agent.Plan
import dev.nickolaj.pocketnix.agent.Updates
import kotlinx.coroutines.runBlocking
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Test

class CheckAndApplyTest {
    private fun state(available: String? = null, plan: Plan? = null, error: String? = null) = Updates(emptyList(), "idle", 1, "/nix/store/a-system", "", available, "local", 0, error, emptyList(), emptyList(), plan = plan)

    private val compiles = state(available = "/nix/store/b-system", plan = Plan(0, listOf("portal-1.0")))

    /** An agent whose check answers [checked] and whose apply answers [applied]. */
    private fun agent(checked: Updates, applied: Updates = state()): suspend (JSONObject) -> Updates = { req -> if (req.getString("type") == "update-check") checked else applied }

    @Test
    fun aDeclinedCompileIsNotDone() {
        val outcome = runBlocking { checkAndApply(agent(compiles), confirm = { false }) }
        assertEquals(UpdateOutcome.Declined, outcome)
    }

    @Test
    fun anAllowedCompileIsDone() {
        val outcome = runBlocking { checkAndApply(agent(compiles), confirm = { true }) }
        assertEquals(UpdateOutcome.Done, outcome)
    }

    @Test
    fun nothingNewerIsDone() {
        assertEquals(UpdateOutcome.Done, runBlocking { checkAndApply(agent(state()), confirm = { false }) })
    }

    @Test
    fun aFailedApplyFails() {
        val outcome = runBlocking { checkAndApply(agent(compiles, state(error = "switch failed")), confirm = { true }) }
        assertEquals(UpdateOutcome.Failed("switch failed"), outcome)
    }
}
