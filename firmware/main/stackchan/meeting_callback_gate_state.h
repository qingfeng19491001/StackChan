#ifndef STACKCHAN_MEETING_CALLBACK_GATE_STATE_H
#define STACKCHAN_MEETING_CALLBACK_GATE_STATE_H

class MeetingCallbackGateState {
public:
    explicit MeetingCallbackGateState(void* owner = nullptr)
        : owner_(owner)
    {
    }

    void* Owner() const { return owner_; }
    unsigned long long BeginGeneration()
    {
        ++generation_;
        if (generation_ == 0) ++generation_;
        return generation_;
    }

    bool TryEnter(unsigned long long generation, void*& owner)
    {
        if (owner_ == nullptr || generation != generation_) return false;
        ++in_flight_;
        owner = owner_;
        return true;
    }

    bool TryEnterCurrent(void*& owner)
    {
        if (owner_ == nullptr) return false;
        ++in_flight_;
        owner = owner_;
        return true;
    }

    void Leave()
    {
        if (in_flight_ > 0) --in_flight_;
    }

    void Disable()
    {
        owner_ = nullptr;
        BeginGeneration();
    }

    bool CanDestroy() const { return in_flight_ == 0; }

private:
    void* owner_ = nullptr;
    unsigned long long generation_ = 0;
    unsigned int in_flight_ = 0;
};

#endif
